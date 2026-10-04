package replay

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func windowsSteamRoots() []string {
	roots := []string{}
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), `C:\Program Files (x86)`, `C:\Program Files`} {
		if base != "" {
			roots = append(roots, filepath.Join(base, "Steam"))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "reg", "query", `HKCU\Software\Valve\Steam`, "/v", "SteamPath").Output()
	if err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if _, path, ok := strings.Cut(line, "REG_SZ"); ok {
				roots = append(roots, strings.TrimSpace(path))
			}
		}
	}
	return roots
}
