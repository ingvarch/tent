package app

import (
	"context"
	"errors"
)

// PayloadOf is payloadOf for the tests of package app_test.
var PayloadOf = payloadOf

// Stands returns the error that an interrupted use case stands for, whose text is only "interrupted", or err when it
// is not one.
func Stands(err error) error {
	if i, ok := errors.AsType[*interrupted](err); ok {
		return i.err
	}
	return err
}

// RunRoll prepares the rolling update of the cluster as RollingUpdate does and carries its steps out, without the
// cluster's lock and without OnRollPlan. It returns what the roll did, and the error that ended it.
func RunRoll(ctx context.Context, s *Service, cluster string, opts RollOptions) (_ RollCounts, err error) {
	defer func() { err = stopped(ctx, err) }()
	l, err := s.layout(ctx, cluster)
	if err != nil {
		return RollCounts{}, err
	}
	r, err := s.prepareRoll(ctx, l, opts, assetCache{})
	if err != nil {
		return RollCounts{}, err
	}
	return r.run(ctx)
}
