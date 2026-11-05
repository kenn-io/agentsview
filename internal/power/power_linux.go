package power

import (
	"os"
	"path/filepath"
	"strconv"
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
		if read("scope") == "Device" {
			continue
		}
		switch read("type") {
		case "Mains", "USB":
			if online, err := strconv.Atoi(read("online")); err == nil && online > 0 {
				return false
			}
		case "Battery":
			if read("status") == "Discharging" {
				discharging = true
			}
		}
	}
	return discharging
}
