//go:build !darwin

package config

// DefaultStateDir follows the FHS: /var/lib is where Linux keeps state a
// service owns.
const DefaultStateDir = "/var/lib/edgepilot"
