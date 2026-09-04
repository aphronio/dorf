package gateway

import "fmt"

type ModelDefaults interface {
	DefaultConnection() (string, error)
	DefaultModel(string) (string, error)
}

// ResolveModel fills omitted selections from the configured connection defaults.
// It supports both local Gateway state and the control reader's remote projection.
func ResolveModel(defaults ModelDefaults, connection, model string) (string, string, error) {
	if defaults == nil {
		return "", "", fmt.Errorf("provider readiness is not configured")
	}
	var err error
	if connection == "" {
		connection, err = defaults.DefaultConnection()
		if err != nil {
			return "", "", err
		}
	}
	if model == "" {
		model, err = defaults.DefaultModel(connection)
		if err != nil {
			return "", "", err
		}
	}
	return connection, model, nil
}
