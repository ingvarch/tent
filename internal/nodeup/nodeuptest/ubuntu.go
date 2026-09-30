package nodeuptest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
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

// unitDir is where the units that tent-node installs live, and packageUnitDir where those of packages live.
const (
	unitDir        = "/etc/systemd/system"
	packageUnitDir = "/usr/lib/systemd/system"
)

// The units of the firewalls that an image may run. Vultr's images run ufw; firewalld is not in Ubuntu's.
const (
	ufw       = "ufw.service"
	firewalld = "firewalld.service"
)

// ufwConfFile is ufw's configuration, and ufwConf that of Vultr's Ubuntu images, which enable ufw.
const (
	ufwConfFile = "/etc/ufw/ufw.conf"
	ufwConf     = `# /etc/ufw/ufw.conf
#

# Set to yes to start on boot. If setting this remotely, be sure to add a rule
# to allow your remote connection before starting ufw. Eg: 'ufw allow 22/tcp'
ENABLED=yes

# Please use the 'ufw-framework' manual page instead of setting this. See the
# manual page for details.
#IPT_SYSCTL=/etc/ufw/sysctl.conf

# Set to one of 'off', 'low', 'medium', 'high' or 'full'
LOGLEVEL=low
`
)

// ufwEnabled matches the setting of ufw.conf that turns ufw on at boot.
var ufwEnabled = regexp.MustCompile(`(?m)^ENABLED=.*$`)

// docker is the unit of Ubuntu's docker.io package, which it puts into /usr/lib/systemd/system.
const docker = "docker.service"

// Commands of dpkg and apt, which the runtime phase runs through env to set DEBIAN_FRONTEND.
const (
	dpkgQuery     = "dpkg-query -W -f=${Status} docker.io"
	dpkgConfigure = "env DEBIAN_FRONTEND=noninteractive dpkg --force-confdef --force-confold --configure -a"
	aptUpdate     = "env DEBIAN_FRONTEND=noninteractive apt-get update"
	aptInstall    = "env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " +
		"-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold docker.io"
)

// lockErrors are the errors of the commands, by command, while another process holds a lock that they do not wait
// for, as apt 2.8 and dpkg 1.22 print them.
var lockErrors = map[string]nodeup.ExitError{
	aptUpdate: {Code: 100, Stderr: "E: Could not get lock /var/lib/apt/lists/lock. It is held by process 1234 " +
		"(apt-get)\nE: Unable to lock directory /var/lib/apt/lists/"},
	dpkgConfigure: {Code: 2, Stderr: "dpkg: error: dpkg frontend lock was locked by another process with pid 1234\n" +
		"Note: removing the lock file is always wrong, can damage the locked area\n" +
		"and the entire system. See <https://wiki.debian.org/Teams/Dpkg/FAQ#db-lock>."},
	aptInstall: {Code: 100, Stderr: "E: Could not get lock /var/cache/apt/archives/lock. It is held by process 1234 " +
		"(apt-get)\nE: Unable to lock directory /var/cache/apt/archives/"},
}

// The services that keep the time: timedatectl manages systemd-timesyncd, which Vultr's Ubuntu 24.04 and 26.04
// images run. Some images run chrony instead.
const (
	timesyncd = "systemd-timesyncd.service"
	chrony    = "chrony.service"
)

// Ubuntu makes fsys and r an Ubuntu 24.04 machine that runs systemd, as tent-node finds one on Vultr, and returns the
// machine's state that its files do not hold. fsys gets OSRelease at /etc/os-release; the directories
// /run/systemd/system, /etc/systemd/system, /etc/modules-load.d, /etc/sysctl.d, /var/lib, /etc/tent, where
// cloud-init writes the NodeConfig, and /opt, with mode 0755 and owned by root:root; and ufw's unit file and its
// configuration, which enables it. It records none of that as changes. r answers as the machine does, from a state
// that the commands change:
//   - systemctl is-enabled, enable, is-active, start and show -p NeedDaemonReload --value of each of the units. The
//     units start disabled and inactive. enable and start fail, as systemd does, when the unit has no file in
//     /etc/systemd/system or /usr/lib/systemd/system; is-enabled then prints not-found and exits with 4, as systemd
//     255 and 259 do.
//   - systemctl daemon-reload, and enable too, read the units' files again. NeedDaemonReload is yes when a unit's file
//     changed or went since systemd read it. systemd compares the files' modification times, and the fake compares
//     their content. systemd reads a file when it is first asked about the unit, or when the unit starts.
//   - timedatectl show -p CanNTP -p NTP, and timedatectl set-ntp true, which starts systemd-timesyncd.service; and
//     systemctl is-active of it and of chrony.service. timedatectl can turn NTP on, and it starts off.
//   - systemctl is-enabled, is-active, disable and disable --now of ufw.service and firewalld.service, whose files
//     packages put into /usr/lib/systemd/system. ufw is enabled and active; firewalld is not installed until
//     Machine.InstallFirewalld. ufw disable writes ENABLED=no into /etc/ufw/ufw.conf, a change that fsys records, and
//     leaves the unit enabled, as ufw does.
//   - nft -f nodeup.FirewallFile loads the ruleset that the hostfirewall phase renders; it fails for a file without
//     tent-node's header and tent's table. nft -j list tables prints the tables as nft 1.0.9 does: its metainfo, and
//     tent's table with the comment of the loaded file once it is loaded. At first it is not, and there are no tables.
//   - dpkg-query -W -f=${Status} docker.io, and dpkg --configure -a, apt-get update and apt-get install of docker.io
//     through env, as the runtime phase runs them. docker.io is not installed: dpkg-query exits with 1, as for a
//     package that dpkg never had. The install puts docker.service into /usr/lib/systemd/system, a change that fsys
//     records, and enables and starts it, as the package's postinst does. The three commands succeed, unless
//     Machine.LockDpkg, LockAptLists or LockAptArchives make them fail. systemctl is-enabled, enable, disable,
//     is-active, start, restart and show -p Job --value answer for docker.service, which has no job until
//     Machine.Reboot queues one.
//
// Other commands succeed with no output, as they do on r.
func Ubuntu(t testing.TB, fsys *FS, r *Runner, units ...string) *Machine {
	t.Helper()
	fsys.AddFile(t, "/etc/os-release", []byte(OSRelease), 0o644, nodeconfig.Owner)
	for _, dir := range []string{
		"/run/systemd/system", unitDir, "/etc/modules-load.d", "/etc/sysctl.d", "/var/lib", "/etc/tent", "/opt",
	} {
		fsys.AddDir(t, dir, 0o755, nodeconfig.Owner)
	}
	fsys.AddFile(t, ufwConfFile, []byte(ufwConf), 0o644, nodeconfig.Owner)
	fsys.AddFile(t, packageUnitDir+"/"+ufw, []byte("[Unit]\nDescription=Uncomplicated firewall\n"), 0o644,
		nodeconfig.Owner)
	s := &systemd{
		fsys: fsys, units: units, read: map[string][]byte{},
		enabled: map[string]bool{ufw: true}, active: map[string]bool{ufw: true}, jobs: map[string]int{},
	}
	for _, unit := range units {
		r.On("systemctl is-enabled "+unit, s.answer(func() ([]byte, error) { return s.isEnabled(unit) }))
		r.On("systemctl enable "+unit, s.answer(func() ([]byte, error) { return s.enable(unit) }))
		r.On("systemctl start "+unit, s.answer(func() ([]byte, error) { return s.start(unit) }))
		r.On("systemctl show -p NeedDaemonReload --value "+unit,
			s.answer(func() ([]byte, error) { return s.needDaemonReload(unit) }))
	}
	for _, unit := range []string{ufw, firewalld} {
		r.On("systemctl is-enabled "+unit, s.answer(func() ([]byte, error) { return s.isEnabled(unit) }))
		r.On("systemctl disable "+unit, s.answer(func() ([]byte, error) { return s.disable(unit, false) }))
		r.On("systemctl disable --now "+unit, s.answer(func() ([]byte, error) { return s.disable(unit, true) }))
	}
	r.On("systemctl is-enabled "+docker, s.answer(func() ([]byte, error) { return s.isEnabled(docker) }))
	r.On("systemctl enable "+docker, s.answer(func() ([]byte, error) { return s.enable(docker) }))
	r.On("systemctl start "+docker, s.answer(func() ([]byte, error) { return s.start(docker) }))
	r.On("systemctl restart "+docker, s.answer(func() ([]byte, error) { return s.restart(docker) }))
	r.On("systemctl disable "+docker, s.answer(func() ([]byte, error) { return s.disable(docker, false) }))
	r.On("systemctl show -p Job --value "+docker, s.answer(func() ([]byte, error) { return s.job(docker) }))
	for _, unit := range append([]string{timesyncd, chrony, ufw, firewalld, docker}, units...) {
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
	r.On("ufw disable", func(context.Context) ([]byte, error) { return disableUFW(fsys) })
	n := &nftables{fsys: fsys}
	r.On("nft -f "+nodeup.FirewallFile, n.answer(n.load))
	r.On("nft -j list tables", n.answer(n.list))
	p := &packages{fsys: fsys, systemd: s, held: map[string]int{}}
	r.On(dpkgQuery, p.answer(p.query))
	r.On(dpkgConfigure, p.whenFree(dpkgConfigure, func() ([]byte, error) { return nil, nil }))
	r.On(aptUpdate, p.whenFree(aptUpdate, func() ([]byte, error) { return nil, nil }))
	r.On(aptInstall, p.whenFree(aptInstall, p.install))
	return &Machine{fsys: fsys, systemd: s, nft: n, packages: p}
}

// Machine is the state of a fake Ubuntu machine that its files do not hold: systemd's, the kernel's nftables tables,
// and dpkg's and apt's.
type Machine struct {
	fsys     *FS
	systemd  *systemd
	nft      *nftables
	packages *packages
}

// InstallFirewalld installs firewalld, enabled and active, as its package leaves it. It records no change.
func (m *Machine) InstallFirewalld(t testing.TB) {
	t.Helper()
	m.fsys.AddFile(t, packageUnitDir+"/"+firewalld, []byte("[Unit]\nDescription=firewalld - dynamic firewall daemon\n"),
		0o644, nodeconfig.Owner)
	m.systemd.mu.Lock()
	defer m.systemd.mu.Unlock()
	m.systemd.enabled[firewalld], m.systemd.active[firewalld] = true, true
}

// LockAptLists makes the next tries runs of apt-get update fail as while another apt, such as unattended-upgrades,
// holds the lock of the package lists: with exit status 100.
func (m *Machine) LockAptLists(tries int) { m.packages.hold(aptUpdate, tries) }

// LockDpkg makes the next tries runs of dpkg --configure -a fail as while another apt holds dpkg's lock: with exit
// status 2.
func (m *Machine) LockDpkg(tries int) { m.packages.hold(dpkgConfigure, tries) }

// LockAptArchives makes the next tries runs of apt-get install fail as while another apt holds the lock of the
// downloaded packages: with exit status 100.
func (m *Machine) LockAptArchives(tries int) { m.packages.hold(aptInstall, tries) }

// Reboot drops the nftables tables, which live only in the kernel, as a reboot does. It leaves docker.service
// inactive, and an enabled one with a start job queued, as tent-node found it on Ubuntu 24.04 (VM check 2026-09-30):
// Docker starts on the same boot as tent-node.service, which is not ordered after it. systemctl start of it runs the
// job. The files and the rest of systemd's state stay as they are.
func (m *Machine) Reboot() {
	m.nft.mu.Lock()
	m.nft.loaded, m.nft.comment = false, ""
	m.nft.mu.Unlock()
	m.systemd.mu.Lock()
	defer m.systemd.mu.Unlock()
	m.systemd.active[docker] = false
	if m.systemd.enabled[docker] {
		m.systemd.lastJob++
		m.systemd.jobs[docker] = m.systemd.lastJob
	}
}

// disableUFW turns ufw off as ufw disable does: it writes ENABLED=no into ufw.conf and leaves the unit enabled.
func disableUFW(fsys *FS) ([]byte, error) {
	conf, err := fsys.ReadFile(ufwConfFile)
	if err != nil {
		return nil, &nodeup.ExitError{Code: 1, Stderr: "ERROR: Couldn't open '" + ufwConfFile + "' for reading"}
	}
	if _, err := fsys.WriteFile(ufwConfFile, ufwEnabled.ReplaceAll(conf, []byte("ENABLED=no")), 0o644,
		nodeconfig.Owner); err != nil {
		return nil, err
	}
	return []byte("Firewall stopped and disabled on system startup\n"), nil
}

// guard is the lock of a fake's state.
type guard struct{ mu sync.Mutex }

// answer returns an Answer that calls f with the state locked.
func (g *guard) answer(f func() ([]byte, error)) Answer {
	return func(context.Context) ([]byte, error) {
		g.mu.Lock()
		defer g.mu.Unlock()
		return f()
	}
}

// tentTable is the head of the table that tent-node's ruleset defines.
const tentTable = "table inet tent {\n"

// nftables is the kernel's nftables on a fake machine, as far as tent's table goes.
type nftables struct {
	guard
	fsys    *FS
	loaded  bool   // whether tent's table is loaded
	comment string // the table's comment
}

// load loads nodeup.FirewallFile as nft -f does, and keeps the comment of tent's table. It takes only a file with
// tent-node's header and tent's table.
func (n *nftables) load() ([]byte, error) {
	data, err := n.fsys.ReadFile(nodeup.FirewallFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, &nodeup.ExitError{
			Code: 1, Stderr: `Error: Could not open file "` + nodeup.FirewallFile + `": No such file or directory`,
		}
	case err != nil:
		return nil, err
	}
	_, body, ok := strings.Cut(string(data), "\n"+tentTable)
	if !ok || !bytes.HasPrefix(data, []byte(nodeconfig.NodeHeader)) {
		return nil, &nodeup.ExitError{Code: 1, Stderr: nodeup.FirewallFile + ": Error: not tent-node's ruleset"}
	}
	first, _, _ := strings.Cut(body, "\n")
	comment, ok := strings.CutPrefix(strings.TrimSpace(first), `comment "`)
	n.loaded, n.comment = true, ""
	if ok {
		n.comment = strings.TrimSuffix(comment, `"`)
	}
	return nil, nil
}

// list prints the tables as nft -j list tables does: its metainfo, then tent's table when it is loaded.
func (n *nftables) list() ([]byte, error) {
	objects := []any{map[string]any{"metainfo": map[string]any{
		"version": "1.0.9", "release_name": "Old Doc Yak #3", "json_schema_version": 1,
	}}}
	if n.loaded {
		table := map[string]any{"family": "inet", "name": "tent", "handle": 3}
		if n.comment != "" {
			table["comment"] = n.comment
		}
		objects = append(objects, map[string]any{"table": table})
	}
	out, err := json.Marshal(map[string]any{"nftables": objects})
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// packages is the state of dpkg and apt on a fake machine, as far as docker.io goes.
type packages struct {
	guard
	fsys    *FS
	systemd *systemd
	docker  bool           // whether docker.io is installed
	held    map[string]int // how many more runs of a command find its lock held, by command
}

// query prints docker.io's status as dpkg-query -W -f=${Status} does, without a final newline.
func (p *packages) query() ([]byte, error) {
	if !p.docker {
		return nil, &nodeup.ExitError{Code: 1, Stderr: "dpkg-query: no packages found matching docker.io"}
	}
	return []byte("install ok installed"), nil
}

// hold makes the next tries runs of the command find its lock held.
func (p *packages) hold(command string, tries int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held[command] = tries
}

// whenFree returns an Answer that fails with the command's lock error while its lock is held, and else calls f, with
// the state locked.
func (p *packages) whenFree(command string, f func() ([]byte, error)) Answer {
	return p.answer(func() ([]byte, error) {
		if p.held[command] > 0 {
			p.held[command]--
			e := lockErrors[command]
			return nil, &e
		}
		return f()
	})
}

// install installs docker.io: its unit file, which its postinst enables and starts.
func (p *packages) install() ([]byte, error) {
	if _, err := p.fsys.WriteFile(packageUnitDir+"/"+docker,
		[]byte("[Unit]\nDescription=Docker Application Container Engine\n"), 0o644, nodeconfig.Owner); err != nil {
		return nil, err
	}
	p.docker = true
	p.systemd.mu.Lock()
	defer p.systemd.mu.Unlock()
	p.systemd.enabled[docker], p.systemd.active[docker] = true, true
	return nil, nil
}

// systemd is the state of systemd on a fake machine.
type systemd struct {
	guard
	fsys    *FS
	units   []string          // the units that it answers for
	read    map[string][]byte // the unit files as systemd read them, by unit
	enabled map[string]bool   // by unit
	active  map[string]bool   // by unit
	jobs    map[string]int    // the id of the job queued for a unit, by unit
	lastJob int               // the id of the last job queued
	ntp     bool              // whether timedatectl turned NTP on
}

// file returns the unit's file, from /etc/systemd/system or else from /usr/lib/systemd/system, and whether it has one.
func (s *systemd) file(unit string) ([]byte, bool) {
	for _, dir := range []string{unitDir, packageUnitDir} {
		if e, ok := s.fsys.Entry(dir + "/" + unit); ok && !e.Dir {
			return e.Data, true
		}
	}
	return nil, false
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

// disable makes the unit not start at boot, and with now stops it too.
func (s *systemd) disable(unit string, now bool) ([]byte, error) {
	if _, ok := s.file(unit); !ok {
		return nil, &nodeup.ExitError{Code: 1, Stderr: "Failed to disable unit: Unit file " + unit + " does not exist."}
	}
	s.enabled[unit] = false
	if now {
		s.active[unit] = false
		delete(s.jobs, unit)
	}
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
	delete(s.jobs, unit)
	return nil, nil
}

// job prints the id of the unit's queued job as systemctl show -p Job --value does, or an empty line.
func (s *systemd) job(unit string) ([]byte, error) {
	if id, ok := s.jobs[unit]; ok {
		return []byte(strconv.Itoa(id) + "\n"), nil
	}
	return []byte("\n"), nil
}

// restart starts the unit, which may run already. It fails, as start does, when the unit has no file.
func (s *systemd) restart(unit string) ([]byte, error) {
	if _, ok := s.file(unit); !ok {
		return nil, &nodeup.ExitError{Code: 5, Stderr: "Failed to restart " + unit + ": Unit " + unit + " not found."}
	}
	return s.start(unit)
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
