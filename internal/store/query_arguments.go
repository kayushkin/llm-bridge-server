package store

import "strconv"

// queryArguments collects the arguments of a query built in parts and names
// each one's placeholder as it is added, so the numbers of $1, $2, … always
// match the order of the arguments, however the parts are joined.
type queryArguments []any

// add appends value and returns its placeholder.
func (arguments *queryArguments) add(value any) string {
	*arguments = append(*arguments, value)
	return "$" + strconv.Itoa(len(*arguments))
}
