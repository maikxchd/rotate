package rollback

import (
	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
)

// Rollback is an abstraction for a configurable stack of rollback functions
type Rollback interface {
	AddStep(step Step) int
	Clear()
	Run() (bool, error)
}

type Step struct {
	Fn          func() error
	Name        string
	StopOnError bool
}

type impl struct {
	steps []Step
}

func New() Rollback {
	return &impl{}
}

// Clear removes all existing steps from the rollback procedure
func (i *impl) Clear() {
	i.steps = nil
}

// AddStep adds a rollback function to the list of rollback steps
func (i *impl) AddStep(step Step) int {
	i.steps = append(i.steps, step)
	return len(i.steps)
}

// Run runs our rollback functions as a stack
func (i *impl) Run() (bool, error) {
	var err error
	for idx := len(i.steps) - 1; idx >= 0; idx-- {
		e := i.steps[idx].Fn()
		if e != nil {
			err = multierror.Append(err, errors.Wrapf(e, "rollback: rollback step %s failed", i.steps[idx].Name))
			if i.steps[idx].StopOnError {
				for j := idx - 1; j >= 0; j-- {
					err = multierror.Append(err, errors.Errorf("rollback: step not done %s", i.steps[j].Name))
				}
				return false, err
			}
		}
	}
	return true, err
}
