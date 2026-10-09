package chromehost

import "golang.org/x/sys/windows/registry"

const RegistrationKey = `Software\Google\Chrome\NativeMessagingHosts\io.kenn.agentsview`

func RegisteredManifest(string) (string, error) {
	return ReadRegistrationKey(RegistrationKey)
}

func ReadRegistrationKey(subkey string) (string, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, subkey, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()
	path, _, err := key.GetStringValue("")
	return path, err
}
