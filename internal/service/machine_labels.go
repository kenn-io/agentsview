package service

import "context"

// MachineLabelCatalog maps machine keys to display labels.
type MachineLabelCatalog map[string]string

// MachineLabelProvider is an optional capability for services that can resolve
// machine keys to display labels.
type MachineLabelProvider interface {
	MachineLabels(context.Context) (MachineLabelCatalog, error)
}

// MachineLabels returns the machine label catalog when svc supports it.
func MachineLabels(
	ctx context.Context, svc SessionService,
) (MachineLabelCatalog, error) {
	capability, ok := svc.(MachineLabelProvider)
	if !ok {
		return nil, nil
	}
	return capability.MachineLabels(ctx)
}
