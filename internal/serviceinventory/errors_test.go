package serviceinventory

import "errors"

func asRequestError(err error, target **RequestError) bool { return errors.As(err, target) }
