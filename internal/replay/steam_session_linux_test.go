package replay

import "testing"

func TestSteamDesktopAndMigrationSelection(t *testing.T) {
	client := steamProcess{pid: 10, args: []string{"/home/user/Steam/ubuntu12_32/steam"}, display: ":1", group: "0::/app/snap.steam.steam-abc.scope"}
	helper := steamProcess{pid: 11, ppid: 10, args: []string{"/home/user/Steam/ubuntu12_64/steamwebhelper"}, group: client.group}
	obs := steamProcess{pid: 20, args: []string{"/usr/bin/obs"}, display: ":20"}
	processes := []steamProcess{client, helper, obs}
	if !desktopSteam(processes) {
		t.Fatal("desktop Steam missed")
	}
	targets, err := steamShutdownTargets(processes)
	if err != nil || !targets[10] || !targets[11] || targets[20] {
		t.Fatal(targets, err)
	}
	processes[0].display = ":20"
	if desktopSteam(processes) {
		t.Fatal("headless misidentified")
	}
	processes = append(processes, steamProcess{pid: 30, args: []string{"/home/user/Steam/steamapps/common/Counter-Strike Global Offensive/game/bin/linuxsteamrt64/cs2"}})
	if _, err := steamShutdownTargets(processes); err == nil {
		t.Fatal("migration would kill a running game")
	}
	if isSteamClient([]string{"/usr/bin/python", "/steam/ubuntu12_32/steam"}) {
		t.Fatal("matched argument instead of process")
	}
}
