package config

// PrepareDraftCredentials gives new drivers the same stable credential
// identity used by Settings, before masked drafts can be renamed or reordered.
func PrepareDraftCredentials(cfg, previous *Config) error {
	return assignCredentialOwners(cfg, previous)
}
