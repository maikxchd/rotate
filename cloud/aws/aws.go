package aws

import (
	"context"
	"fmt"
	"time"

	"code.justin.tv/safety/k8s-rot8/rollback"

	"code.justin.tv/safety/k8s-rot8/cloud"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/pkg/errors"
)

type Clients struct {
	autoScaling autoscalingiface.AutoScalingAPI
	ec2         ec2iface.EC2API
}

func New(configProvider client.ConfigProvider) *Clients {
	return &Clients{
		autoScaling: autoscaling.New(configProvider),
		ec2:         ec2.New(configProvider),
	}
}

type instanceInfo struct {
	asgInfo *autoscaling.Instance
	ec2Info *ec2.Instance
}

const healthy = "Healthy"

func (i *instanceInfo) isHealthy() bool {
	return aws.StringValue(i.asgInfo.HealthStatus) == healthy
}

func (i *instanceInfo) IsActive() bool {
	state := aws.StringValue(i.ec2Info.State.Name)
	return state == "running"

}

func (i *instanceInfo) age() time.Time {
	return aws.TimeValue(i.ec2Info.LaunchTime)
}

// ID returns the unique identifier for this instance
func (i *instanceInfo) ID() string {
	return aws.StringValue(i.asgInfo.InstanceId)
}

// Name returns the unique name for this instance
func (i *instanceInfo) Name() string {
	return aws.StringValue(i.ec2Info.PrivateDnsName)
}

type asg struct {
	clients   *Clients
	Group     *autoscaling.Group
	Instances map[string]*instanceInfo
}

// FindRotationTarget finds the instance which we will shutdown in this cluster
func (a *asg) FindRotationTarget(nodeToFind string) (cloud.Instance, error) {
	var selection *instanceInfo
	for _, instance := range a.Instances {
		if aws.StringValue(instance.ec2Info.InstanceId) == nodeToFind ||
			aws.StringValue(instance.ec2Info.PrivateDnsName) == nodeToFind {
			return instance, nil
		}

		if selection == nil {
			selection = instance
		} else if selection.isHealthy() && !instance.isHealthy() {
			selection = instance
		} else if a.hasCurrentLaunchConfig(selection) && !a.hasCurrentLaunchConfig(instance) {
			selection = instance
		} else if instance.age().Before(selection.age()) {
			selection = instance
		}

	}

	if selection == nil {
		return nil, errors.New("no rotation target: empty asg")
	}
	if nodeToFind != "" {
		return nil, errors.New("cannot find rotation target")
	}

	return selection, nil
}

func (a *asg) hasCurrentLaunchConfig(i *instanceInfo) bool {
	return aws.StringValue(i.asgInfo.LaunchConfigurationName) == aws.StringValue(a.Group.LaunchConfigurationName)
}

// DescribeASGAndInstances fetches all of the information about an ASG and its Instances
func (c *Clients) DescribeASGsAndInstances(ctx context.Context, asgIDs []string) ([]*asg, error) {
	var asgList []*autoscaling.Group
	err := c.autoScaling.DescribeAutoScalingGroupsPagesWithContext(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: aws.StringSlice(asgIDs),
	}, func(output *autoscaling.DescribeAutoScalingGroupsOutput, lastPage bool) bool {
		asgList = append(asgList, output.AutoScalingGroups...)
		return true
	})
	if err != nil {
		return nil, errors.Wrap(err, "aws: failed to describe autoscaling groups")
	}

	var asgs []*asg
	for _, asgDesc := range asgList {
		instanceMap := make(map[string]*instanceInfo)
		var instanceIDs []*string
		for _, instance := range asgDesc.Instances {
			instanceIDs = append(instanceIDs, instance.InstanceId)
			instanceMap[aws.StringValue(instance.InstanceId)] = &instanceInfo{asgInfo: instance}
		}

		err := c.ec2.DescribeInstancesPagesWithContext(ctx,
			&ec2.DescribeInstancesInput{InstanceIds: instanceIDs},
			func(output *ec2.DescribeInstancesOutput, b bool) bool {
				for _, reservation := range output.Reservations {
					for _, instance := range reservation.Instances {
						id := aws.StringValue(instance.InstanceId)
						if v, ok := instanceMap[id]; ok {
							v.ec2Info = instance
						}
					}
				}
				return true
			})
		if err != nil {
			return nil, errors.Wrap(err, "aws: failed to describe ec2 Instances")
		}

		asgs = append(asgs, &asg{
			clients:   c,
			Group:     asgDesc,
			Instances: instanceMap,
		})
	}

	return asgs, nil
}

var terminate = aws.String("Terminate")

// SuspendASGTermination is a function to set an ASG to no longer terminate nodes
func (c *Clients) SuspendASGTermination(ctx context.Context, group *autoscaling.Group) (rollback.Step, error) {
	_, err := c.autoScaling.SuspendProcessesWithContext(ctx, &autoscaling.ScalingProcessQuery{
		AutoScalingGroupName: group.AutoScalingGroupName,
		ScalingProcesses:     []*string{terminate},
	})
	if err != nil {
		return rollback.Step{}, errors.Wrapf(err,
			"asg:%s suspend termination failure", aws.StringValue(group.AutoScalingGroupName))
	}

	return rollback.Step{
		Fn: func() error {
			err := c.ResumeASGTermination(context.Background(), group)
			return err
		},
		StopOnError: true,
		Name: fmt.Sprintf(
			"rollback-asg-%s-resume-termination",
			aws.StringValue(group.AutoScalingGroupName)),
	}, nil
}

// ResumeASGTermination is a function to allow the ASG to terminate nodes again
func (c *Clients) ResumeASGTermination(ctx context.Context, group *autoscaling.Group) error {
	_, err := c.autoScaling.ResumeProcessesWithContext(ctx, &autoscaling.ScalingProcessQuery{
		AutoScalingGroupName: group.AutoScalingGroupName,
		ScalingProcesses:     []*string{terminate},
	})
	return errors.Wrapf(err, "asg:%s resume termination failure", aws.StringValue(group.AutoScalingGroupName))
}

// ScaleUpASGBy1 increases the desired and maximum capacity of the ASG so that we get a scale up
func (c *Clients) ScaleUpASGBy1(ctx context.Context, group *autoscaling.Group) (rollback.Step, error) {
	_, err := c.autoScaling.UpdateAutoScalingGroupWithContext(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: group.AutoScalingGroupName,
		MinSize:              aws.Int64(aws.Int64Value(group.DesiredCapacity)),
		DesiredCapacity:      aws.Int64(aws.Int64Value(group.DesiredCapacity) + 1),
		MaxSize:              aws.Int64(aws.Int64Value(group.MaxSize) + 1),
	})
	if err != nil {
		return rollback.Step{}, errors.Wrapf(err, "asg:%s failed to scale up asg",
			aws.StringValue(group.AutoScalingGroupName))

	}

	return rollback.Step{
		Fn: func() error {
			err := c.ReturnToOriginalScale(context.Background(), group)
			return err
		},
		StopOnError: true,
		Name: fmt.Sprintf(
			"rollback-asg-%s-return-to-original-scale",
			aws.StringValue(group.AutoScalingGroupName)),
	}, nil
}

// ReturnToOriginalScale sets the desired and maximum capacity to that of what we have in the original desc
func (c *Clients) ReturnToOriginalScale(ctx context.Context, group *autoscaling.Group) error {
	_, err := c.autoScaling.UpdateAutoScalingGroupWithContext(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: group.AutoScalingGroupName,
		DesiredCapacity:      group.DesiredCapacity,
		MaxSize:              group.MaxSize,
		MinSize:              group.MinSize,
	})
	return errors.Wrapf(err, "asg:%s failed to return to original scale",
		aws.StringValue(group.AutoScalingGroupName))
}

// TerminateInstanceInASG kills and tries to downsize the asg at the same time
func (c *Clients) TerminateInstanceInASG(ctx context.Context, group *autoscaling.Group, instance cloud.Instance) error {
	_, err := c.autoScaling.TerminateInstanceInAutoScalingGroupWithContext(ctx,
		&autoscaling.TerminateInstanceInAutoScalingGroupInput{
			InstanceId:                     aws.String(instance.ID()),
			ShouldDecrementDesiredCapacity: aws.Bool(true),
		})

	return errors.Wrapf(err,
		"asg:%s instance:%s - failed to terminate instance in asg",
		aws.StringValue(group.AutoScalingGroupName),
		instance.ID)
}

// WaitUntilScalingDone waits until we have completed the scaling action
func (c *Clients) WaitingUntilScalingDone(ctx context.Context, group *asg, waitPeriod time.Duration) (int, error) {

	expected := int(aws.Int64Value(group.Group.DesiredCapacity) + 1)
	for {
		output, err := c.autoScaling.DescribeAutoScalingGroupsWithContext(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
			AutoScalingGroupNames: []*string{group.Group.AutoScalingGroupName},
		})
		if err != nil || len(output.AutoScalingGroups) == 0 {
			return 0, errors.Wrap(err, "describe failure")
		}

		if len(output.AutoScalingGroups[0].Instances) >= expected {
			allReady := true
			for _, inst := range output.AutoScalingGroups[0].Instances {
				allReady = allReady && aws.StringValue(inst.HealthStatus) == healthy
			}
			if allReady {
				return len(output.AutoScalingGroups[0].Instances), nil
			}
		}

		select {
		case <-ctx.Done():
			return 0, errors.Wrap(ctx.Err(), "asg not found")
		case <-time.After(waitPeriod):
		}
	}
}
