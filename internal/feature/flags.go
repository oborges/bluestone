package feature

import (
	"github.com/oborges/bluestone/internal/config"
)

// FeatureFlags controls experimental and new features
type FeatureFlags struct {
	// UseStagingPath enables the new staging-based write path
	// When false, uses legacy direct-to-COS path
	UseStagingPath bool
}

// LoadFeatureFlags creates feature flags from configuration
func LoadFeatureFlags(cfg *config.Config) *FeatureFlags {
	return &FeatureFlags{
		UseStagingPath: cfg.Staging.Enabled,
	}
}

// IsStagingEnabled returns true if staging path should be used
func (ff *FeatureFlags) IsStagingEnabled() bool {
	return ff.UseStagingPath
}

// Made with Bob
