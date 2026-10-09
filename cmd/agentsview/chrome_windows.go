package main

import (
	"go.kenn.io/agentsview/internal/chromehost"
	"golang.org/x/sys/windows/registry"
)

func registerChromeHost(path string) error {
	return registerChromeHostKey(chromehost.RegistrationKey, path)
}

func registerChromeHostKey(subkey, path string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, subkey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue("", path)
}
