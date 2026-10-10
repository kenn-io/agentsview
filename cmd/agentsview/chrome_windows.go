package main

import (
	"golang.org/x/sys/windows/registry"
)

const RegistrationKey = `Software\Google\Chrome\NativeMessagingHosts\io.kenn.agentsview`

func registerChromeHost(path string) error {
	return registerChromeHostKey(RegistrationKey, path)
}

func registerChromeHostKey(subkey, path string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, subkey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue("", path)
}
