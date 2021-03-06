package store

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// NewStoreFunc is a function for opening a databse.
type NewStoreFunc func(path string) (KVStore, error)

type Registration struct {
	Name        string // unique name
	Title       string // human-readable name
	FactoryFunc NewStoreFunc
}

var registry = make(map[string]*Registration)

func Register(reg *Registration) {
	if reg.Name == "" {
		zlog.Fatal("name cannot be blank")
	} else if _, ok := registry[reg.Name]; ok {
		zlog.Fatal("already registered", zap.String("name", reg.Name))
	}
	registry[reg.Name] = reg
}

func isRegistered(schemeName string) bool {
	_, isRegistered := registry[schemeName]
	return isRegistered
}

func New(dsn string, opts ...Option) (KVStore, error) {
	chunks := strings.Split(dsn, ":")
	reg, found := registry[chunks[0]]
	if !found {
		return nil, fmt.Errorf("no such kv store registered %q", chunks[0])
	}
	store, err := reg.FactoryFunc(dsn)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt.apply(store)
	}
	return store, nil
}

// ByName returns a registered store driver
func ByName(name string) *Registration {
	r, ok := registry[name]
	if !ok {
		return nil
	}
	return r
}
