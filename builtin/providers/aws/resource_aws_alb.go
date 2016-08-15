package aws

import (
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"
)

func resourceAwsAlb() *schema.Resource {
	return &schema.Resource{
		Create: resourceAwsAlbCreate,
		Read:   resourceAwsAlbRead,
		Update: resourceAwsAlbUpdate,
		Delete: resourceAwsAlbDelete,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: validateElbName,
			},

			"internal": {
				Type:     schema.TypeBool,
				Optional: true,
				ForceNew: true,
				Computed: true,
			},

			"security_groups": {
				Type:     schema.TypeSet,
				Elem:     &schema.Schema{Type: schema.TypeString},
				ForceNew: true,
				Optional: true,
				Set:      schema.HashString,
			},

			"subnets": {
				Type:     schema.TypeSet,
				Elem:     &schema.Schema{Type: schema.TypeString},
				ForceNew: true,
				Required: true,
				Set:      schema.HashString,
			},

			"access_logs": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"bucket": {
							Type:     schema.TypeString,
							Required: true,
						},
						"prefix": {
							Type:     schema.TypeString,
							Optional: true,
						},
					},
				},
			},

			"enable_deletion_protection": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},

			"idle_timeout": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  60,
			},

			"vpc_id": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"zone_id": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"dns_name": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"tags": tagsSchema(),
		},
	}
}

func resourceAwsAlbCreate(d *schema.ResourceData, meta interface{}) error {
	elbconn := meta.(*AWSClient).elbv2conn

	elbOpts := &elbv2.CreateLoadBalancerInput{
		Name: aws.String(d.Get("name").(string)),
		Tags: tagsFromMapELBv2(d.Get("tags").(map[string]interface{})),
	}

	if scheme, ok := d.GetOk("internal"); ok && scheme.(bool) {
		elbOpts.Scheme = aws.String("internal")
	}

	if v, ok := d.GetOk("security_groups"); ok {
		elbOpts.SecurityGroups = expandStringList(v.(*schema.Set).List())
	}

	if v, ok := d.GetOk("subnets"); ok {
		elbOpts.Subnets = expandStringList(v.(*schema.Set).List())
	}

	log.Printf("[DEBUG] ALB create configuration: %#v", elbOpts)
	var albArn string
	err := resource.Retry(1*time.Minute, func() *resource.RetryError {
		resp, err := elbconn.CreateLoadBalancer(elbOpts)
		if err != nil {
			return resource.NonRetryableError(err)
		}

		if len(resp.LoadBalancers) != 1 {
			return resource.NonRetryableError(fmt.Errorf(
				"No loadbalancers returned following creation of %s", d.Get("name").(string)))
		}

		albArn = *resp.LoadBalancers[0].LoadBalancerArn
		return nil
	})

	if err != nil {
		return err
	}

	d.SetId(albArn)
	log.Printf("[INFO] ALB ID: %s", d.Id())

	return resourceAwsAlbUpdate(d, meta)
}

func resourceAwsAlbRead(d *schema.ResourceData, meta interface{}) error {
	elbconn := meta.(*AWSClient).elbv2conn
	albArn := d.Id()

	describeAlbOpts := &elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{aws.String(albArn)},
	}

	describeResp, err := elbconn.DescribeLoadBalancers(describeAlbOpts)
	if err != nil {
		if isLoadBalancerNotFound(err) {
			// The ALB is gone now, so just remove it from the state
			d.SetId("")
			return nil
		}

		return errwrap.Wrapf("Error retrieving ALB: {{err}}", err)
	}
	if len(describeResp.LoadBalancers) != 1 {
		return fmt.Errorf("Unable to find ALB: %#v", describeResp.LoadBalancers)
	}

	alb := describeResp.LoadBalancers[0]

	d.Set("name", alb.LoadBalancerName)
	d.Set("internal", (alb.Scheme != nil && *alb.Scheme == "internal"))
	d.Set("security_groups", flattenStringList(alb.SecurityGroups))
	d.Set("subnets", flattenSubnetsFromAvailabilityZones(alb.AvailabilityZones))
	d.Set("vpc_id", alb.VpcId)
	d.Set("zone_id", alb.CanonicalHostedZoneId)
	d.Set("dns_name", alb.DNSName)

	respTags, err := elbconn.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{alb.LoadBalancerArn},
	})
	if err != nil {
		return errwrap.Wrapf("Error retrieving ALB Tags: {{err}}", err)
	}

	var et []*elbv2.Tag
	if len(respTags.TagDescriptions) > 0 {
		et = respTags.TagDescriptions[0].Tags
	}
	d.Set("tags", tagsToMapELBv2(et))

	attributesResp, err := elbconn.DescribeLoadBalancerAttributes(&elbv2.DescribeLoadBalancerAttributesInput{
		LoadBalancerArn: aws.String(d.Id()),
	})
	if err != nil {
		return errwrap.Wrapf("Error retrieving ALB Attributes: {{err}}", err)
	}

	accessLogMap := map[string]interface{}{}
	for _, attr := range attributesResp.Attributes {
		switch *attr.Key {
		case "access_logs.s3.bucket":
			accessLogMap["bucket"] = *attr.Value
		case "access_logs.s3.prefix":
			accessLogMap["prefix"] = *attr.Value
		case "idle_timeout.timeout_seconds":
			timeout, err := strconv.Atoi(*attr.Value)
			if err != nil {
				return errwrap.Wrapf("Error parsing ALB timeout: {{err}}", err)
			}
			log.Printf("[DEBUG] Setting ALB Timeout Seconds: %d", timeout)
			d.Set("idle_timeout", timeout)
		case "deletion_protection.enabled":
			protectionEnabled := (*attr.Value) == "true"
			log.Printf("[DEBUG] Setting ALB Deletion Protection Enabled: %t", protectionEnabled)
			d.Set("enable_deletion_protection", protectionEnabled)
		}
	}

	log.Printf("[DEBUG] Setting ALB Access Logs: %#v", accessLogMap)
	if accessLogMap["bucket"] != "" || accessLogMap["prefix"] != "" {
		d.Set("access_logs", []interface{}{accessLogMap})
	} else {
		d.Set("access_logs", []interface{}{})
	}

	return nil
}

func resourceAwsAlbUpdate(d *schema.ResourceData, meta interface{}) error {
	elbconn := meta.(*AWSClient).elbv2conn

	attributes := make([]*elbv2.LoadBalancerAttribute, 0)

	if d.HasChange("access_logs") {
		logs := d.Get("access_logs").([]interface{})
		if len(logs) == 1 {
			log := logs[0].(map[string]interface{})

			attributes = append(attributes,
				&elbv2.LoadBalancerAttribute{
					Key:   aws.String("access_logs.s3.enabled"),
					Value: aws.String("true"),
				},
				&elbv2.LoadBalancerAttribute{
					Key:   aws.String("access_logs.s3.bucket"),
					Value: aws.String(log["bucket"].(string)),
				})

			if prefix, ok := log["prefix"]; ok {
				attributes = append(attributes, &elbv2.LoadBalancerAttribute{
					Key:   aws.String("access_logs.s3.prefix"),
					Value: aws.String(prefix.(string)),
				})
			}
		} else if len(logs) == 0 {
			attributes = append(attributes, &elbv2.LoadBalancerAttribute{
				Key:   aws.String("access_logs.s3.enabled"),
				Value: aws.String("false"),
			})
		}
	}

	if d.HasChange("enable_deletion_protection") {
		attributes = append(attributes, &elbv2.LoadBalancerAttribute{
			Key:   aws.String("deletion_protection.enabled"),
			Value: aws.String(fmt.Sprintf("%t", d.Get("enable_deletion_protection").(bool))),
		})
	}

	if d.HasChange("idle_timeout") {
		attributes = append(attributes, &elbv2.LoadBalancerAttribute{
			Key:   aws.String("idle_timeout.timeout_seconds"),
			Value: aws.String(fmt.Sprintf("%d", d.Get("idle_timeout").(int))),
		})
	}

	if len(attributes) != 0 {
		input := &elbv2.ModifyLoadBalancerAttributesInput{
			LoadBalancerArn: aws.String(d.Id()),
			Attributes:      attributes,
		}

		log.Printf("[DEBUG] ALB Modify Load Balancer Attributes Request: %#v", input)
		_, err := elbconn.ModifyLoadBalancerAttributes(input)
		if err != nil {
			return fmt.Errorf("Failure configuring ALB attributes: %s", err)
		}
	}

	return resourceAwsAlbRead(d, meta)
}

func resourceAwsAlbDelete(d *schema.ResourceData, meta interface{}) error {
	albconn := meta.(*AWSClient).elbv2conn

	log.Printf("[INFO] Deleting ALB: %s", d.Id())

	// Destroy the load balancer
	deleteElbOpts := elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(d.Id()),
	}
	if _, err := albconn.DeleteLoadBalancer(&deleteElbOpts); err != nil {
		return fmt.Errorf("Error deleting ALB: %s", err)
	}

	return nil
}

// tagsToMapELBv2 turns the list of tags into a map.
func tagsToMapELBv2(ts []*elbv2.Tag) map[string]string {
	result := make(map[string]string)
	for _, t := range ts {
		result[*t.Key] = *t.Value
	}

	return result
}

// tagsFromMapELBv2 returns the tags for the given map of data.
func tagsFromMapELBv2(m map[string]interface{}) []*elbv2.Tag {
	var result []*elbv2.Tag
	for k, v := range m {
		result = append(result, &elbv2.Tag{
			Key:   aws.String(k),
			Value: aws.String(v.(string)),
		})
	}

	return result
}

// flattenSubnetsFromAvailabilityZones creates a slice of strings containing the subnet IDs
// for the ALB based on the AvailabilityZones structure returned by the API.
func flattenSubnetsFromAvailabilityZones(availabilityZones []*elbv2.AvailabilityZone) []string {
	var result []string
	for _, az := range availabilityZones {
		result = append(result, *az.SubnetId)
	}
	return result
}
