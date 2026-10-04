package dock

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const userServiceName = "dock.service"

func DaemonCommand(args []string, executable string) error {
	if len(args) == 0 {
		return errors.New("use dock daemon install, uninstall, enable, disable, start, stop, or status")
	}
	unitPath, err := userServicePath()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		abs, err := filepath.Abs(executable)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(unitPath), 0700); err != nil {
			return err
		}
		unit := fmt.Sprintf("[Unit]\nDescription=Dock local container gateway\n\n[Service]\nType=simple\nExecStart=%s serve\nRestart=on-failure\nRestartSec=3\nKillSignal=SIGTERM\nTimeoutStopSec=25\n\n[Install]\nWantedBy=default.target\n", systemdQuote(abs))
		if err := os.WriteFile(unitPath, []byte(unit), 0600); err != nil {
			return err
		}
		if err := runSystemctl("daemon-reload"); err != nil {
			return err
		}
		fmt.Printf("Installed user service at %s. Run 'dock daemon enable' to start it at login.\n", unitPath)
		return nil
	case "uninstall":
		if err := runSystemctl("disable", "--now", userServiceName); err != nil {
			return err
		}
		if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := runSystemctl("daemon-reload"); err != nil {
			return err
		}
		fmt.Println("Removed Dock user service.")
		return nil
	case "enable", "disable", "start", "stop", "status":
		return runSystemctl(append(args, userServiceName)...)
	default:
		return fmt.Errorf("unknown daemon action %q", args[0])
	}
}

func userServicePath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "systemd", "user", userServiceName), nil
}

func runSystemctl(args ...string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl is not installed")
	}
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func systemdQuote(value string) string {
	return strings.NewReplacer("\\", "\\\\", " ", "\\x20", "\t", "\\x09").Replace(value)
}
