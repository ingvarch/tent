package nodeuptest

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/ingvarch/tent/internal/nodeconfig"
	"github.com/ingvarch/tent/internal/nodeup"
)

// OSRelease is /etc/os-release of Ubuntu 24.04, which Ubuntu gives a fake machine.
const OSRelease = `PRETTY_NAME="Ubuntu 24.04.3 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.3 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
SUPPORT_URL="https://help.ubuntu.com/"
BUG_REPORT_URL="https://bugs.launchpad.net/ubuntu/"
PRIVACY_POLICY_URL="https://www.ubuntu.com/legal/terms-and-policies/privacy-policy"
UBUNTU_CODENAME=noble
LOGO=ubuntu-logo
`

// unitDir is where the units that tent-node installs live.
const unitDir = "/etc/systemd/system"

// The services that keep the time: timedatectl manages systemd-timesyncd, which Vultr's Ubuntu 24.04 and 26.04
// images run. Some images run chrony instead.
const (
	timesyncd = "systemd-timesyncd.service"
	chrony    = "chrony.service"
)

// Ubuntu makes fsys and r an Ubuntu 24.04 machine that runs systemd, as tent-node finds one. fsys gets OSRelease at
// /etc/os-release, and the directories /run/systemd/system, /etc/systemd/system, /etc/modules-load.d, /etc/sysctl.d
// and /var/lib, with mode 0755 and owned by root:root, as changes it does not record. r answers as systemd does, from
// a state that the commands change:
//   - systemctl is-enabled, enable, is-active, start and show -p NeedDaemonReload --value of each of the units. The
//     units start disabled and inactive. enable and start fail, as systemd does, when the unit has no file in
//     /etc/systemd/system; is-enabled then prints not-found and exits with 4, as systemd 255 and 259 do.
//   - systemctl daemon-reload, and enable too, read the units' files again. NeedDaemonReload is yes when a unit's file
//     changed or went since systemd read it. systemd compares the files' modification times, and the fake compares
//     their content. systemd reads a file when it is first asked about the unit, or when the unit starts.
//   - timedatectl show -p CanNTP -p NTP, and timedatectl set-ntp true, which starts systemd-timesyncd.service; and
//     systemctl is-active of it and of chrony.service. timedatectl can turn NTP on, and it starts off.
//
// Other commands succeed with no output, as they do on r.
func Ubuntu(t testing.TB, fsys *FS, r *Runner, units ...string) {
	t.Helper()
	fsys.AddFile(t, "/etc/os-release", []byte(OSRelease), 0o644, nodeconfig.Owner)
	for _, dir := range []string{"/run/systemd/system", unitDir, "/etc/modules-load.d", "/etc/sysctl.d", "/var/lib"} {
		fsys.AddDir(t, dir, 0o755, nodeconfig.Owner)
	}
	s := &systemd{
		fsys: fsys, units: units, read: map[string][]byte{}, enabled: map[string]bool{}, active: map[string]bool{},
	}
	for _, unit := range units {
		r.On("systemctl is-enabled "+unit, s.answer(func() ([]byte, error) { return s.isEnabled(unit) }))
		r.On("systemctl enable "+unit, s.answer(func() ([]byte, error) { return s.enable(unit) }))
		r.On("systemctl start "+unit, s.answer(func() ([]byte, error) { return s.start(unit) }))
		r.On("systemctl show -p NeedDaemonReload --value "+unit,
			s.answer(func() ([]byte, error) { return s.needDaemonReload(unit) }))
	}
	for _, unit := range append([]string{timesyncd, chrony}, units...) {
		r.On("systemctl is-active "+unit, s.answer(func() ([]byte, error) { return s.isActive(unit) }))
	}
	r.On("systemctl daemon-reload", s.answer(func() ([]byte, error) {
		s.reload()
		return nil, nil
	}))
	r.On("timedatectl show -p CanNTP -p NTP", s.answer(func() ([]byte, error) {
		return []byte("CanNTP=yes\nNTP=" + yesNo(s.ntp) + "\n"), nil
	}))
	r.On("timedatectl set-ntp true", s.answer(func() ([]byte, error) {
		s.ntp, s.active[timesyncd] = true, true
		return nil, nil
	}))
}

// systemd is the state of systemd on a fake machine.
type systemd struct {
	mu      sync.Mutex
	fsys    *FS
	units   []string          // the units that it answers for
	read    map[string][]byte // the unit files as systemd read them, by unit
	enabled map[string]bool   // by unit
	active  map[string]bool   // by unit
	ntp     bool              // whether timedatectl turned NTP on
}

// answer returns an Answer that calls f with the state locked.
func (s *systemd) answer(f func() ([]byte, error)) Answer {
	return func(context.Context) ([]byte, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return f()
	}
}

// file returns the unit's file, and whether it has one.
func (s *systemd) file(unit string) ([]byte, bool) {
	e, ok := s.fsys.Entry(unitDir + "/" + unit)
	return e.Data, ok && !e.Dir
}

// reload reads the units' files again, as daemon-reload does.
func (s *systemd) reload() {
	for _, unit := range s.units {
		if data, ok := s.file(unit); ok {
			s.read[unit] = data
		} else {
			delete(s.read, unit)
		}
	}
}

func (s *systemd) isEnabled(unit string) ([]byte, error) {
	switch _, ok := s.file(unit); {
	case !ok:
		return []byte("not-found\n"), &nodeup.ExitError{Code: 4}
	case s.enabled[unit]:
		return []byte("enabled\n"), nil
	}
	return []byte("disabled\n"), &nodeup.ExitError{Code: 1}
}

func (s *systemd) isActive(unit string) ([]byte, error) {
	if s.active[unit] {
		return []byte("active\n"), nil
	}
	return []byte("inactive\n"), &nodeup.ExitError{Code: 3}
}

func (s *systemd) enable(unit string) ([]byte, error) {
	if _, ok := s.file(unit); !ok {
		return nil, &nodeup.ExitError{Code: 1, Stderr: "Failed to enable unit: Unit file " + unit + " does not exist."}
	}
	s.enabled[unit] = true
	s.reload()
	return nil, nil
}

func (s *systemd) start(unit string) ([]byte, error) {
	data, ok := s.file(unit)
	if !ok {
		return nil, &nodeup.ExitError{Code: 5, Stderr: "Failed to start " + unit + ": Unit " + unit + " not found."}
	}
	if _, read := s.read[unit]; !read {
		s.read[unit] = data
	}
	s.active[unit] = true
	return nil, nil
}

func (s *systemd) needDaemonReload(unit string) ([]byte, error) {
	data, ok := s.file(unit)
	old, read := s.read[unit]
	switch {
	case !read && ok:
		s.read[unit] = data // systemd reads a unit when first asked about it
		return []byte("no\n"), nil
	case !read:
		return []byte("no\n"), nil
	}
	return []byte(yesNo(!ok || !bytes.Equal(old, data)) + "\n"), nil
}

// yesNo returns yes for true and no for false, as systemd prints them.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
