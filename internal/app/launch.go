package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"starbound-modmanager/internal/i18n"
)

const starboundSteamURL = "steam://rungameid/211820"

// LaunchDirect starts the installed game executable without asking Steam to launch it.
func (m *Manager) LaunchDirect() error {
	m.mu.RLock()
	gameDir := m.gameDir
	m.mu.RUnlock()
	if gameDir == "" {
		return i18n.NewError(i18n.KeyErrGameRequired)
	}

	launchPath, appBundle, err := directLaunchPath(gameDir)
	if err != nil {
		return err
	}
	var command *exec.Cmd
	if appBundle {
		command = exec.Command("open", "-a", launchPath)
	} else {
		command = exec.Command(launchPath)
	}
	command.Dir = gameDir
	if runtime.GOOS == "linux" {
		libraryPath := filepath.Dir(launchPath)
		if inherited := os.Getenv("LD_LIBRARY_PATH"); inherited != "" {
			libraryPath += string(os.PathListSeparator) + inherited
		}
		environment := os.Environ()
		replaced := false
		for i, variable := range environment {
			if strings.HasPrefix(variable, "LD_LIBRARY_PATH=") {
				environment[i] = "LD_LIBRARY_PATH=" + libraryPath
				replaced = true
				break
			}
		}
		if !replaced {
			environment = append(environment, "LD_LIBRARY_PATH="+libraryPath)
		}
		command.Env = environment
	}
	if err := command.Start(); err != nil {
		return i18n.WrapError(i18n.KeyErrLaunchDirect, err)
	}
	go func() { _ = command.Wait() }()
	return nil
}

// LaunchViaSteam asks the operating system to open Starbound's Steam app link.
func (m *Manager) LaunchViaSteam() error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", starboundSteamURL)
	case "darwin":
		command = exec.Command("open", starboundSteamURL)
	default:
		if steam, err := exec.LookPath("steam"); err == nil {
			command = exec.Command(steam, "-applaunch", "211820")
		} else {
			command = exec.Command("xdg-open", starboundSteamURL)
		}
	}
	if err := command.Start(); err != nil {
		return i18n.WrapError(i18n.KeyErrLaunchSteam, err)
	}
	go func() { _ = command.Wait() }()
	return nil
}

func directLaunchPath(gameDir string) (string, bool, error) {
	candidates := []string{}
	switch runtime.GOOS {
	case "windows":
		candidates = append(candidates,
			filepath.Join(gameDir, "win64", "starbound.exe"),
			filepath.Join(gameDir, "win32", "starbound.exe"),
			filepath.Join(gameDir, "starbound.exe"),
		)
	case "darwin":
		candidates = append(candidates,
			filepath.Join(gameDir, "osx", "Starbound.app"),
			filepath.Join(gameDir, "Starbound.app"),
		)
		if strings.EqualFold(filepath.Ext(gameDir), ".app") {
			candidates = append(candidates, gameDir)
		}
	case "linux":
		candidates = append(candidates,
			filepath.Join(gameDir, "linux", "starbound"),
			filepath.Join(gameDir, "starbound"),
		)
	default:
		return "", false, i18n.NewError(i18n.KeyErrLaunchUnsupportedOS, runtime.GOOS)
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if runtime.GOOS == "darwin" && info.IsDir() && strings.EqualFold(filepath.Ext(candidate), ".app") {
			return candidate, true, nil
		}
		if info.Mode().IsRegular() {
			return candidate, false, nil
		}
	}
	return "", false, i18n.NewError(i18n.KeyErrLaunchExecutable, gameDir)
}
