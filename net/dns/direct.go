// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"inet.af/netaddr"
	"tailscale.com/util/dnsname"
)

const (
	backupConf = "/etc/resolv.pre-tailscale-backup.conf"
	resolvConf = "/etc/resolv.conf"
)

// writeResolvConf writes DNS configuration in resolv.conf format to the given writer.
func writeResolvConf(w io.Writer, servers []netaddr.IP, domains []dnsname.FQDN) {
	io.WriteString(w, "# resolv.conf(5) file generated by tailscale\n")
	io.WriteString(w, "# DO NOT EDIT THIS FILE BY HAND -- CHANGES WILL BE OVERWRITTEN\n\n")
	for _, ns := range servers {
		io.WriteString(w, "nameserver ")
		io.WriteString(w, ns.String())
		io.WriteString(w, "\n")
	}
	if len(domains) > 0 {
		io.WriteString(w, "search")
		for _, domain := range domains {
			io.WriteString(w, " ")
			io.WriteString(w, domain.WithoutTrailingDot())
		}
		io.WriteString(w, "\n")
	}
}

func readResolv(r io.Reader) (config OSConfig, err error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "nameserver") {
			nameserver := strings.TrimPrefix(line, "nameserver")
			nameserver = strings.TrimSpace(nameserver)
			ip, err := netaddr.ParseIP(nameserver)
			if err != nil {
				return OSConfig{}, err
			}
			config.Nameservers = append(config.Nameservers, ip)
			continue
		}

		if strings.HasPrefix(line, "search") {
			domain := strings.TrimPrefix(line, "search")
			domain = strings.TrimSpace(domain)
			fqdn, err := dnsname.ToFQDN(domain)
			if err != nil {
				return OSConfig{}, fmt.Errorf("parsing search domains %q: %w", line, err)
			}
			config.SearchDomains = append(config.SearchDomains, fqdn)
			continue
		}
	}

	return config, nil
}

func (m directManager) readResolvFile(path string) (OSConfig, error) {
	b, err := m.fs.ReadFile(path)
	if err != nil {
		return OSConfig{}, err
	}
	return readResolv(bytes.NewReader(b))
}

// readResolvConf reads DNS configuration from /etc/resolv.conf.
func (m directManager) readResolvConf() (OSConfig, error) {
	return m.readResolvFile(resolvConf)
}

// resolvOwner returns the apparent owner of the resolv.conf
// configuration in bs - one of "resolvconf", "systemd-resolved" or
// "NetworkManager", or "" if no known owner was found.
func resolvOwner(bs []byte) string {
	b := bytes.NewBuffer(bs)
	for {
		line, err := b.ReadString('\n')
		if err != nil {
			return ""
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line[0] != '#' {
			// First non-empty, non-comment line. Assume the owner
			// isn't hiding further down.
			return ""
		}

		if strings.Contains(line, "systemd-resolved") {
			return "systemd-resolved"
		} else if strings.Contains(line, "NetworkManager") {
			return "NetworkManager"
		} else if strings.Contains(line, "resolvconf") {
			return "resolvconf"
		}
	}
}

// isResolvedRunning reports whether systemd-resolved is running on the system,
// even if it is not managing the system DNS settings.
func isResolvedRunning() bool {
	if runtime.GOOS != "linux" {
		return false
	}

	// systemd-resolved is never installed without systemd.
	_, err := exec.LookPath("systemctl")
	if err != nil {
		return false
	}

	// is-active exits with code 3 if the service is not active.
	err = exec.Command("systemctl", "is-active", "systemd-resolved.service").Run()

	return err == nil
}

// directManager is an OSConfigurator which replaces /etc/resolv.conf with a file
// generated from the given configuration, creating a backup of its old state.
//
// This way of configuring DNS is precarious, since it does not react
// to the disappearance of the Tailscale interface.
// The caller must call Down before program shutdown
// or as cleanup if the program terminates unexpectedly.
type directManager struct {
	fs wholeFileFS
}

func newDirectManager() directManager {
	return directManager{fs: directFS{}}
}

func newDirectManagerOnFS(fs wholeFileFS) directManager {
	return directManager{fs: fs}
}

// ownedByTailscale reports whether /etc/resolv.conf seems to be a
// tailscale-managed file.
func (m directManager) ownedByTailscale() (bool, error) {
	isRegular, err := m.fs.Stat(resolvConf)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !isRegular {
		return false, nil
	}
	bs, err := m.fs.ReadFile(resolvConf)
	if err != nil {
		return false, err
	}
	if bytes.Contains(bs, []byte("generated by tailscale")) {
		return true, nil
	}
	return false, nil
}

// backupConfig creates or updates a backup of /etc/resolv.conf, if
// resolv.conf does not currently contain a Tailscale-managed config.
func (m directManager) backupConfig() error {
	if _, err := m.fs.Stat(resolvConf); err != nil {
		if os.IsNotExist(err) {
			// No resolv.conf, nothing to back up. Also get rid of any
			// existing backup file, to avoid restoring something old.
			m.fs.Remove(backupConf)
			return nil
		}
		return err
	}

	owned, err := m.ownedByTailscale()
	if err != nil {
		return err
	}
	if owned {
		return nil
	}

	return m.fs.Rename(resolvConf, backupConf)
}

func (m directManager) restoreBackup() error {
	if _, err := m.fs.Stat(backupConf); err != nil {
		if os.IsNotExist(err) {
			// No backup, nothing we can do.
			return nil
		}
		return err
	}
	owned, err := m.ownedByTailscale()
	if err != nil {
		return err
	}
	if _, err := m.fs.Stat(resolvConf); err != nil && !os.IsNotExist(err) {
		return err
	}
	resolvConfExists := !os.IsNotExist(err)

	if resolvConfExists && !owned {
		// There's already a non-tailscale config in place, get rid of
		// our backup.
		m.fs.Remove(backupConf)
		return nil
	}

	// We own resolv.conf, and a backup exists.
	if err := m.fs.Rename(backupConf, resolvConf); err != nil {
		return err
	}

	return nil
}

func (m directManager) SetDNS(config OSConfig) error {
	if config.IsZero() {
		if err := m.restoreBackup(); err != nil {
			return err
		}
	} else {
		if err := m.backupConfig(); err != nil {
			return err
		}

		buf := new(bytes.Buffer)
		writeResolvConf(buf, config.Nameservers, config.SearchDomains)
		if err := atomicWriteFile(m.fs, resolvConf, buf.Bytes(), 0644); err != nil {
			return err
		}
	}

	// We might have taken over a configuration managed by resolved,
	// in which case it will notice this on restart and gracefully
	// start using our configuration. This shouldn't happen because we
	// try to manage DNS through resolved when it's around, but as a
	// best-effort fallback if we messed up the detection, try to
	// restart resolved to make the system configuration consistent.
	if isResolvedRunning() && !runningAsGUIDesktopUser() {
		exec.Command("systemctl", "restart", "systemd-resolved.service").Run()
	}

	return nil
}

func (m directManager) SupportsSplitDNS() bool {
	return false
}

func (m directManager) GetBaseConfig() (OSConfig, error) {
	owned, err := m.ownedByTailscale()
	if err != nil {
		return OSConfig{}, err
	}
	fileToRead := resolvConf
	if owned {
		fileToRead = backupConf
	}

	return m.readResolvFile(fileToRead)
}

func (m directManager) Close() error {
	// We used to keep a file for the tailscale config and symlinked
	// to it, but then we stopped because /etc/resolv.conf being a
	// symlink to surprising places breaks snaps and other sandboxing
	// things. Clean it up if it's still there.
	m.fs.Remove("/etc/resolv.tailscale.conf")

	if _, err := m.fs.Stat(backupConf); err != nil {
		if os.IsNotExist(err) {
			// No backup, nothing we can do.
			return nil
		}
		return err
	}
	owned, err := m.ownedByTailscale()
	if err != nil {
		return err
	}
	_, err = m.fs.Stat(resolvConf)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	resolvConfExists := !os.IsNotExist(err)

	if resolvConfExists && !owned {
		// There's already a non-tailscale config in place, get rid of
		// our backup.
		m.fs.Remove(backupConf)
		return nil
	}

	// We own resolv.conf, and a backup exists.
	if err := m.fs.Rename(backupConf, resolvConf); err != nil {
		return err
	}

	if isResolvedRunning() && !runningAsGUIDesktopUser() {
		exec.Command("systemctl", "restart", "systemd-resolved.service").Run() // Best-effort.
	}

	return nil
}

func atomicWriteFile(fs wholeFileFS, filename string, data []byte, perm os.FileMode) error {
	var randBytes [12]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return fmt.Errorf("atomicWriteFile: %w", err)
	}

	tmpName := fmt.Sprintf("%s.%x.tmp", filename, randBytes[:])
	defer fs.Remove(tmpName)

	if err := fs.WriteFile(tmpName, data, perm); err != nil {
		return fmt.Errorf("atomicWriteFile: %w", err)
	}
	return fs.Rename(tmpName, filename)
}

// wholeFileFS is a high-level file system abstraction designed just for use
// by directManager, with the goal that it is easy to implement over wsl.exe.
//
// All name parameters are absolute paths.
type wholeFileFS interface {
	Stat(name string) (isRegular bool, err error)
	Rename(oldName, newName string) error
	Remove(name string) error
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, contents []byte, perm os.FileMode) error
}

// directFS is a wholeFileFS implemented directly on the OS.
type directFS struct {
	// prefix is file path prefix.
	//
	// All name parameters are absolute paths so this is typically a
	// testing temporary directory like "/tmp".
	prefix string
}

func (fs directFS) path(name string) string { return filepath.Join(fs.prefix, name) }

func (fs directFS) Stat(name string) (isRegular bool, err error) {
	fi, err := os.Stat(fs.path(name))
	if err != nil {
		return false, err
	}
	return fi.Mode().IsRegular(), nil
}

func (fs directFS) Rename(oldName, newName string) error {
	return os.Rename(fs.path(oldName), fs.path(newName))
}

func (fs directFS) Remove(name string) error { return os.Remove(fs.path(name)) }

func (fs directFS) ReadFile(name string) ([]byte, error) {
	return ioutil.ReadFile(fs.path(name))
}

func (fs directFS) WriteFile(name string, contents []byte, perm os.FileMode) error {
	return ioutil.WriteFile(fs.path(name), contents, perm)
}

// runningAsGUIDesktopUser reports whether it seems that this code is
// being run as a regular user on a Linux desktop. This is a quick
// hack to fix Issue 2672 where PolicyKit pops up a GUI dialog asking
// to proceed we do a best effort attempt to restart
// systemd-resolved.service. There's surely a better way.
func runningAsGUIDesktopUser() bool {
	return os.Getuid() != 0 && os.Getenv("DISPLAY") != ""
}
