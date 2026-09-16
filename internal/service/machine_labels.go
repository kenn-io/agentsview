package service

import "context"

// MachineLabelCatalog is an optional capability for services that can resolve
// machine keys to display labels.
type MachineLabelCatalog interface {
	MachineLabels(context.Context) (map[string]string, error)
}

// MachineLabels returns the machine label catalog when svc supports it.
func MachineLabels(
	ctx context.Context, svc SessionService,
) (map[string]string, error) {
	capability, ok := svc.(MachineLabelCatalog)
	if !ok {
		return nil, nil
	}
	return capability.MachineLabels(ctx)
}
