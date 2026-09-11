package github

import "errors"

func errorsAs(err error, target **StatusError) bool { return errors.As(err, target) }
