package power

import (
	"os"
	"path/filepath"
	"strings"
)

var powerSupplyRoot = "/sys/class/power_supply"

func onBattery() bool {
	supplies, err := os.ReadDir(powerSupplyRoot)
	if err != nil {
		return false
	}
	discharging := false
	for _, supply := range supplies {
		read := func(name string) string {
			value, _ := os.ReadFile(filepath.Join(powerSupplyRoot, supply.Name(), name))
			return strings.TrimSpace(string(value))
		}
		switch read("type") {
		case "Mains", "USB":
			if read("online") == "1" {
				return false
			}
		case "Battery":
			if read("scope") != "Device" && read("status") == "Discharging" {
				discharging = true
			}
		}
	}
	return discharging
}
