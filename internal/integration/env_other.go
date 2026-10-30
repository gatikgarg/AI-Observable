//go:build !windows

package integration

import "errors"

func setUserEnv(string, string) error {
	return errors.New("automatic user environment persistence is not implemented on this OS")
}

func getUserEnv(string) (string, error) {
	return "", errors.New("persisted user environment is not available on this OS")
}

func unsetUserEnv(string) error {
	return errors.New("automatic user environment persistence is not implemented on this OS")
}
