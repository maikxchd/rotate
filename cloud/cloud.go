package cloud

import "context"

// Cloud is the abstract representation of the capabilities of a cloud platform
type Cloud interface {
	DescribeASGsAndInstances(ctx context.Context, asgIDs []string) ([]ASG, error)
}

// ASG is the abstract representation of an auto-scaling group in memory
type ASG interface {
	FindRotationTarget() (Instance, error)
}

// Instance is a simple representation of an instance within the cluster
type Instance interface {
	ID() string
	Name() string
}
