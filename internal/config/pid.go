package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const pidFileName = "mailflow.pid"

func pidPath(configDir string) string {
	return filepath.Join(configDir, pidFileName)
}

func WritePID(configDir string) error {
	pid := os.Getpid()
	return os.WriteFile(pidPath(configDir), []byte(fmt.Sprintf("%d", pid)), 0o644)
}

func ReadPID(configDir string) (int, error) {
	data, err := os.ReadFile(pidPath(configDir))
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, fmt.Errorf("invalid pid file: %w", err)
	}
	return pid, nil
}

func RemovePID(configDir string) {
	_ = os.Remove(pidPath(configDir))
}
