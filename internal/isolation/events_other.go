//go:build !darwin || !cgo

package isolation

func observeHost() (func(map[string]bool) (bool, error), func(), error) {
	return nil, nil, errHostEvents
}
