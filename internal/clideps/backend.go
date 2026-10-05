package clideps

import (
	"fmt"
	"os"

	"nrtn.dev/catalyst/kpr/internal/config"
)

// ResolveStoreBackend derives the state backend from explicit
// signals, never resolved values (KPR_REDIS_ADDR carries a default
// that must not count as a choice): KPR_STORE, when set, is
// authoritative and must agree with backend-specific variables;
// otherwise KPR_STORE_DIR alone selects file and KPR_REDIS_ADDR
// alone selects redis; silence selects file, the zero-dependency
// default. Empty counts as unset throughout. Mixed signals refuse
// instead of guessing.
func ResolveStoreBackend() (backend, dir string, err error) {
	storeVar, storeSet := os.LookupEnv(config.EnvStore)
	dirVar, dirSet := os.LookupEnv(config.EnvStoreDir)
	redisVar, redisSet := os.LookupEnv(config.EnvRedisAddr)
	if storeVar == "" {
		storeSet = false
	}
	if dirVar == "" {
		dirSet = false
	}
	if redisVar == "" {
		redisSet = false
	}
	if storeSet {
		switch storeVar {
		case "file":
			if redisSet {
				return "", "", fmt.Errorf("KPR_STORE=file conflicts with %s: unset one", config.EnvRedisAddr)
			}
			if !dirSet {
				dirVar = config.DefaultStoreDir
			}
			return "file", dirVar, nil
		case "redis":
			if dirSet {
				return "", "", fmt.Errorf("KPR_STORE=redis conflicts with %s: unset one", config.EnvStoreDir)
			}
			return "redis", "", nil
		default:
			return "", "", fmt.Errorf("unknown KPR_STORE=%q: want file or redis", storeVar)
		}
	}
	if dirSet {
		return "file", dirVar, nil
	}
	if redisSet {
		return "redis", "", nil
	}
	return "file", config.DefaultStoreDir, nil
}
