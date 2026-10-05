package app

import "errors"

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
