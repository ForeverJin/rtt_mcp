package jlink

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// defaultLibCandidates returns the per-platform filenames and standard install
// locations of the SEGGER J-Link shared library. Order matters: the most common
// location is tried first.
func defaultLibCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		// SEGGER ships the 64-bit export set as JLink_x64.dll and the 32-bit set
		// as JLinkARM.dll; loading the wrong bitness fails with "not a valid
		// Win32 application". Pick by process architecture so the first candidate
		// matches the build.
		if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
			// SEGGER's installer lands in a version-suffixed folder
			// (JLink_V950, JLink_V796g, …), not a bare "JLink". Glob every
			// SEGGER subfolder so we find whatever version is installed.
			cands := append([]string{}, globSegger("JLink_x64.dll")...)
			cands = append(cands,
				`C:\Program Files\SEGGER\JLink\JLink_x64.dll`,
				`C:\Program Files\SEGGER\JLink\JLinkARM.dll`,
				`JLink_x64.dll`,
			)
			return cands
		}
		cands := append([]string{}, globSegger("JLinkARM.dll")...)
		cands = append(cands,
			`C:\Program Files (x86)\SEGGER\JLink\JLinkARM.dll`,
			`C:\Program Files\SEGGER\JLink\JLinkARM.dll`,
			`JLinkARM.dll`,
		)
		return cands
	case "darwin":
		return []string{
			"/Applications/SEGGER/JLink/libjlinkarm.dylib",
			expandHome("~/Applications/SEGGER/JLink/libjlinkarm.dylib"),
			"/opt/homebrew/lib/libjlinkarm.dylib",
			"/usr/local/lib/libjlinkarm.dylib",
			"libjlinkarm.dylib",
		}
	case "linux":
		return []string{
			"/opt/SEGGER/JLink/libjlinkarm.so",
			"/usr/lib/libjlinkarm.so",
			"/usr/local/lib/libjlinkarm.so",
			expandHome("~/SEGGER/JLink/libjlinkarm.so"),
			"libjlinkarm.so",
		}
	default:
		return nil
	}
}

// globSegger returns JLink DLLs inside any subfolder of the SEGGER install
// directories (Program Files and Program Files (x86)), so versioned installs
// like JLink_V950 are found regardless of the folder name. Returns matches in
// sorted (glob) order.
func globSegger(dll string) []string {
	var out []string
	for _, base := range []string{
		`C:\Program Files\SEGGER`,
		`C:\Program Files (x86)\SEGGER`,
	} {
		matches, _ := filepath.Glob(filepath.Join(base, "*", dll))
		out = append(out, matches...)
	}
	return out
}

// loadJLinkLib opens the SEGGER shared library. If explicit is non-empty it is
// used directly (ahead of defaults); otherwise each candidate is tried via the
// OS-specific openHandle. A descriptive error — including a platform-specific
// download hint — is returned when nothing loads, so callers can surface a
// helpful message instead of a raw loader failure.
func loadJLinkLib(explicit string) (uintptr, error) {
	candidates := defaultLibCandidates()
	if explicit != "" {
		candidates = append([]string{explicit}, candidates...)
	}
	var tried []string
	for _, path := range candidates {
		tried = append(tried, path)
		handle, ok := openHandle(path)
		if ok {
			return handle, nil
		}
		// Missing file is expected for the later candidates; keep trying.
	}
	return 0, fmt.Errorf("SEGGER J-Link library not found. Tried:\n  %s\n\n%s",
		strings.Join(tried, "\n  "), seggerDownloadHint())
}

func seggerDownloadHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "Install SEGGER J-Link: https://www.segger.com/downloads/jlink/ (JLink_macOS.pkg),\nor `brew install --cask segger-jlink`. Then set JLINK_LIB_PATH if it lands elsewhere."
	case "linux":
		return "Install SEGGER J-Link: https://www.segger.com/downloads/jlink/ (.deb/.rpm/tar),\nor set JLINK_LIB_PATH to the installed libjlinkarm.so path."
	default:
		return "Install SEGGER J-Link: https://www.segger.com/downloads/jlink/ (JLink_Windows.exe),\nor set JLINK_LIB_PATH to the installed JLinkARM.dll path."
	}
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return home + p[1:]
}
