package arena

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"runtime/debug"
)

// testerFingerprint describes this controller build. It is stored with each run.
func testerFingerprint(docker *Docker) map[string]any {
	info := map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "architecture": runtime.GOARCH}
	if image := os.Getenv("ARENA_CONTROL_IMAGE"); image != "" {
		info["image"] = image
	}
	if path, err := os.Executable(); err == nil {
		if file, err := os.Open(path); err == nil {
			hash := sha256.New()
			if _, err := io.Copy(hash, file); err == nil {
				info["binarySha256"] = hex.EncodeToString(hash.Sum(nil))
			}
			file.Close()
		}
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		info["buildSettings"] = build.Settings
	}
	if docker != nil {
		info["docker"] = docker.serverInfo
	}
	return info
}
