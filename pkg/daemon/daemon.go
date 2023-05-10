/*
 * Copyright (c) 2020. Ant Group. All rights reserved.
 * Copyright (c) 2022. Nydus Developers. All rights reserved.
 *
 * SPDX-License-Identifier: Apache-2.0
 */

package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pkg/errors"

	"github.com/containerd/containerd/log"

	"github.com/containerd/nydus-snapshotter/config"
	"github.com/containerd/nydus-snapshotter/config/daemonconfig"
	"github.com/containerd/nydus-snapshotter/pkg/daemon/types"
	"github.com/containerd/nydus-snapshotter/pkg/errdefs"
	"github.com/containerd/nydus-snapshotter/pkg/supervisor"
	"github.com/containerd/nydus-snapshotter/pkg/utils/erofs"
	"github.com/containerd/nydus-snapshotter/pkg/utils/mount"
	"github.com/containerd/nydus-snapshotter/pkg/utils/retry"
)

const (
	APISocketFileName   = "api.sock"
	SharedNydusDaemonID = "shared_daemon"
)

type NewDaemonOpt func(d *Daemon) error

type States struct {
	// A unique ID generated by daemon manager to identify the nydusd instance.
	ID          string
	ProcessID   int
	APISocket   string
	LogDir      string
	LogLevel    string
	LogToStdout bool
	DaemonMode  config.DaemonMode
	FsDriver    string
	// Host kernel mountpoint, only applies to fuse fs driver. The fscache fs driver
	// doesn't need a host kernel mountpoint.
	Mountpoint string
	ThreadNum  int
	// Where the configuration file resides, all rafs instances share the same configuration template
	ConfigDir      string
	SupervisorPath string
}

// TODO: Record queried nydusd state
type Daemon struct {
	States States

	mu sync.Mutex
	// FsInstances map[int]*Rafs
	// should be persisted to DB
	// maps to at least one rafs instance.
	// It is possible to be empty after the daemon object is created.
	Instances rafsSet

	// client will be rebuilt on Reconnect, skip marshal/unmarshal
	client NydusdClient
	// Protect nydusd http client
	cmu sync.Mutex
	// Nil means this daemon object has no supervisor
	Supervisor *supervisor.Supervisor
	Config     daemonconfig.DaemonConfig

	// How much CPU nydusd is utilizing when starts since full prefetch might
	// consume many CPU cycles
	StartupCPUUtilization float64
	Version               types.BuildTimeInfo

	ref int32
	// Cache the nydusd daemon state to avoid frequently querying nydusd by API.
	state types.DaemonState
}

func (d *Daemon) Lock() {
	d.mu.Lock()
}

func (d *Daemon) Unlock() {
	d.mu.Unlock()
}

func (d *Daemon) ID() string {
	return d.States.ID
}

func (d *Daemon) Pid() int {
	return d.States.ProcessID
}

func (d *Daemon) IncRef() {
	atomic.AddInt32(&d.ref, 1)
}

func (d *Daemon) DecRef() int32 {
	return atomic.AddInt32(&d.ref, -1)
}

func (d *Daemon) GetRef() int32 {
	return atomic.LoadInt32(&d.ref)
}

func (d *Daemon) HostMountpoint() (mnt string) {
	// Identify a shared nydusd for multiple rafs instances.
	mnt = d.States.Mountpoint
	return
}

// Each nydusd daemon has a copy of configuration json file.
func (d *Daemon) ConfigFile(instanceID string) string {
	if instanceID == "" {
		return filepath.Join(d.States.ConfigDir, "config.json")
	}
	return filepath.Join(d.States.ConfigDir, instanceID, "config.json")
}

// NydusdThreadNum returns how many working threads are needed of a single nydusd
func (d *Daemon) NydusdThreadNum() int {
	return d.States.ThreadNum
}

func (d *Daemon) GetAPISock() string {
	return d.States.APISocket
}

func (d *Daemon) LogFile() string {
	return filepath.Join(d.States.LogDir, "nydusd.log")
}

func (d *Daemon) AddInstance(r *Rafs) {
	d.Instances.Add(r)
	d.IncRef()
	r.DaemonID = d.ID()
}

func (d *Daemon) RemoveInstance(snapshotID string) {
	d.Instances.Remove(snapshotID)
	d.DecRef()
}

// Nydusd daemon current working state by requesting to nydusd:
// 1. INIT
// 2. READY: All needed resources are ready.
// 3. RUNNING
func (d *Daemon) GetState() (types.DaemonState, error) {
	c, err := d.GetClient()
	if err != nil {
		return types.DaemonStateUnknown, errors.Wrapf(err, "get daemon state")
	}
	info, err := c.GetDaemonInfo()
	if err != nil {
		return types.DaemonStateUnknown, err
	}

	st := info.DaemonState()

	d.Lock()
	d.state = st
	d.Version = info.DaemonVersion()
	d.Unlock()

	return st, nil
}

// Return the cached nydusd working status, no API is invoked.
func (d *Daemon) State() types.DaemonState {
	d.Lock()
	defer d.Unlock()
	return d.state
}

// Reset the cached nydusd working status
func (d *Daemon) ResetState() {
	d.Lock()
	defer d.Unlock()
	d.state = types.DaemonStateUnknown
}

// Waits for some time until daemon reaches the expected state.
// For example:
//  1. INIT
//  2. READY
//  3. RUNNING
func (d *Daemon) WaitUntilState(expected types.DaemonState) error {
	return retry.Do(func() error {
		if expected == d.State() {
			return nil
		}

		state, err := d.GetState()
		if err != nil {
			return errors.Wrapf(err, "wait until daemon is %s", expected)
		}

		if state != expected {
			return errors.Errorf("daemon %s is not %s yet, current state %s",
				d.ID(), expected, state)
		}

		return nil
	},
		retry.LastErrorOnly(true),
		retry.Attempts(20), // totally wait for 2 seconds, should be enough
		retry.Delay(100*time.Millisecond),
	)
}

func (d *Daemon) IsSharedDaemon() bool {
	if d.States.DaemonMode != "" {
		return d.States.DaemonMode == config.DaemonModeShared
	}

	return d.HostMountpoint() == config.GetRootMountpoint()
}

func (d *Daemon) SharedMount(rafs *Rafs) error {
	client, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "mount instance %s", rafs.SnapshotID)
	}

	defer d.SendStates()

	if d.States.FsDriver == config.FsDriverFscache {
		if err := d.sharedErofsMount(rafs); err != nil {
			return errors.Wrapf(err, "mount erofs")
		}
		return nil
	}

	bootstrap, err := rafs.BootstrapFile()
	if err != nil {
		return err
	}

	c, err := daemonconfig.NewDaemonConfig(d.States.FsDriver, d.ConfigFile(rafs.SnapshotID))
	if err != nil {
		return errors.Wrapf(err, "Failed to reload instance configuration %s",
			d.ConfigFile(rafs.SnapshotID))
	}

	cfg, err := c.DumpString()
	if err != nil {
		return errors.Wrap(err, "dump instance configuration")
	}

	err = client.Mount(rafs.RelaMountpoint(), bootstrap, cfg)
	if err != nil {
		return errors.Wrapf(err, "mount rafs instance")
	}

	return nil
}

func (d *Daemon) SharedUmount(rafs *Rafs) error {
	c, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "umount instance %s", rafs.SnapshotID)
	}

	defer d.SendStates()

	if d.States.FsDriver == config.FsDriverFscache {
		if err := d.sharedErofsUmount(rafs); err != nil {
			return errors.Wrapf(err, "failed to erofs mount")
		}
		return nil
	}

	return c.Umount(rafs.RelaMountpoint())
}

func (d *Daemon) sharedErofsMount(rafs *Rafs) error {
	client, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "bind blob %s", d.ID())
	}

	// TODO: Why fs cache needing this work dir?
	if err := os.MkdirAll(rafs.FscacheWorkDir(), 0755); err != nil {
		return errors.Wrapf(err, "failed to create fscache work dir %s", rafs.FscacheWorkDir())
	}

	c, err := daemonconfig.NewDaemonConfig(d.States.FsDriver, d.ConfigFile(rafs.SnapshotID))
	if err != nil {
		log.L.Errorf("Failed to reload daemon configuration %s, %s", d.ConfigFile(rafs.SnapshotID), err)
		return err
	}

	cfgStr, err := c.DumpString()
	if err != nil {
		return err
	}

	if err := client.BindBlob(cfgStr); err != nil {
		return errors.Wrapf(err, "request to bind fscache blob")
	}

	mountPoint := rafs.GetMountpoint()
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return errors.Wrapf(err, "create mountpoint %s", mountPoint)
	}

	bootstrapPath, err := rafs.BootstrapFile()
	if err != nil {
		return err
	}
	fscacheID := erofs.FscacheID(rafs.SnapshotID)

	cfg := c.(*daemonconfig.FscacheDaemonConfig)
	rafs.AddAnnotation(AnnoFsCacheDomainID, cfg.DomainID)
	rafs.AddAnnotation(AnnoFsCacheID, fscacheID)

	if err := erofs.Mount(bootstrapPath, cfg.DomainID, fscacheID, mountPoint); err != nil {
		if !errdefs.IsErofsMounted(err) {
			return errors.Wrapf(err, "mount erofs to %s", mountPoint)
		}
		// When snapshotter exits (either normally or abnormally), it will not have a
		// chance to umount erofs mountpoint, so if snapshotter resumes running and mount
		// again (by a new request to create container), it will need to ignore the mount
		// error `device or resource busy`.
		log.L.Warnf("erofs mountpoint %s has been mounted", mountPoint)
	}

	return nil
}

func (d *Daemon) sharedErofsUmount(rafs *Rafs) error {
	c, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "unbind blob %s", d.ID())
	}
	domainID := rafs.Annotations[AnnoFsCacheDomainID]
	fscacheID := rafs.Annotations[AnnoFsCacheID]

	if err := c.UnbindBlob(domainID, fscacheID); err != nil {
		return errors.Wrapf(err, "request to unbind fscache blob, domain %s, fscache %s", domainID, fscacheID)
	}

	mountpoint := rafs.GetMountpoint()
	if err := erofs.Umount(mountpoint); err != nil {
		return errors.Wrapf(err, "umount erofs %s mountpoint, %s", err, mountpoint)
	}

	// delete fscache bootstrap cache file
	// erofs generate fscache cache file for bootstrap with fscacheID
	if err := c.UnbindBlob("", fscacheID); err != nil {
		log.L.Warnf("delete bootstrap %s err %s", fscacheID, err)
	}

	return nil
}

func (d *Daemon) SendStates() {
	su := d.Supervisor
	if su != nil {
		// TODO: This should be optional by checking snapshotter's configuration.
		// FIXME: Is it possible the states are overwritten during two API mounts.
		// FIXME: What if nydusd does not support sending states.
		err := su.FetchDaemonStates(func() error {
			if err := d.doSendStates(); err != nil {
				return errors.Wrapf(err, "send daemon %s states", d.ID())
			}
			return nil
		})
		if err != nil {
			log.L.Warnf("Daemon %s does not support sending states, %v", d.ID(), err)
		}
	}
}

func (d *Daemon) doSendStates() error {
	c, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "send states %s", d.ID())
	}

	if err := c.SendFd(); err != nil {
		return errors.Wrap(err, "request to send states")
	}

	return nil
}

func (d *Daemon) TakeOver() error {
	c, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "takeover daemon %s", d.ID())
	}

	if err := c.TakeOver(); err != nil {
		return errors.Wrap(err, "request to take over")
	}

	return nil
}

func (d *Daemon) Start() error {
	c, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "start service")
	}

	if err := c.Start(); err != nil {
		return errors.Wrap(err, "request to start service")
	}

	return nil
}

func (d *Daemon) Exit() error {
	c, err := d.GetClient()
	if err != nil {
		return errors.Wrapf(err, "start service")
	}

	if err := c.Exit(); err != nil {
		return errors.Wrap(err, "request to exit service")
	}

	return nil
}

func (d *Daemon) GetFsMetrics(sid string) (*types.FsMetrics, error) {
	c, err := d.GetClient()
	if err != nil {
		return nil, errors.Wrapf(err, "get fs metrics")
	}

	return c.GetFsMetrics(sid)
}

func (d *Daemon) GetInflightMetrics() (*types.InflightMetrics, error) {
	c, err := d.GetClient()
	if err != nil {
		return nil, errors.Wrapf(err, "get inflight metrics")
	}

	return c.GetInflightMetrics()
}

func (d *Daemon) GetDaemonInfo() (*types.DaemonInfo, error) {
	c, err := d.GetClient()
	if err != nil {
		return nil, errors.Wrapf(err, "get daemon information")
	}

	return c.GetDaemonInfo()
}

func (d *Daemon) GetCacheMetrics(sid string) (*types.CacheMetrics, error) {
	c, err := d.GetClient()
	if err != nil {
		return nil, errors.Wrapf(err, "get cache metrics")
	}
	return c.GetCacheMetrics(sid)
}

func (d *Daemon) GetClient() (NydusdClient, error) {
	d.cmu.Lock()
	defer d.cmu.Unlock()

	if err := d.ensureClientUnlocked(); err != nil {
		return nil, err
	}

	return d.client, nil
}

func (d *Daemon) ResetClient() {
	d.cmu.Lock()
	d.client = nil
	d.cmu.Unlock()
}

// The client should be locked outside
func (d *Daemon) ensureClientUnlocked() error {
	if d.client == nil {
		sock := d.GetAPISock()
		// The socket file may be residual from a dead nydusd
		err := WaitUntilSocketExisted(sock, d.Pid())
		if err != nil {
			return errors.Wrapf(errdefs.ErrNotFound, "daemon socket %s", sock)
		}
		client, err := NewNydusClient(sock)
		if err != nil {
			return errors.Wrapf(err, "create daemon %s client", d.ID())
		}
		d.client = client
	}
	return nil
}

func (d *Daemon) Terminate() error {
	// if we found pid here, we need to kill and wait process to exit, Pid=0 means somehow we lost
	// the daemon pid, so that we can't kill the process, just roughly umount the mountpoint
	d.Lock()
	defer d.Unlock()

	if d.Pid() > 0 {
		p, err := os.FindProcess(d.Pid())
		if err != nil {
			return errors.Wrapf(err, "find process %d", d.Pid())
		}
		if err = p.Signal(syscall.SIGTERM); err != nil {
			return errors.Wrapf(err, "send SIGTERM signal to process %d", d.Pid())
		}
	}

	return nil
}

func (d *Daemon) Wait() error {
	// if we found pid here, we need to kill and wait process to exit, Pid=0 means somehow we lost
	// the daemon pid, so that we can't kill the process, just roughly umount the mountpoint
	d.Lock()
	defer d.Unlock()

	if d.Pid() > 0 {
		p, err := os.FindProcess(d.Pid())
		if err != nil {
			return errors.Wrapf(err, "find process %d", d.Pid())
		}

		// if nydus-snapshotter restarts, it will break the relationship between nydusd and
		// nydus-snapshotter, p.Wait() will return err, so here should exclude this case
		if _, err = p.Wait(); err != nil && !errors.Is(err, syscall.ECHILD) {
			log.L.Errorf("failed to process wait, %v", err)
		} else if d.HostMountpoint() != "" || config.GetFsDriver() != config.FsDriverFscache {
			// No need to umount if the nydusd never performs mount. In other word, it does not
			// associate with a host mountpoint.
			if err := mount.WaitUntilUnmounted(d.HostMountpoint()); err != nil {
				log.L.WithError(err).Errorf("umount %s", d.HostMountpoint())
			}
		}
	}

	return nil
}

// When daemon dies, clean up its vestige before start a new one.
func (d *Daemon) ClearVestige() {
	mounter := mount.Mounter{}
	if d.States.FsDriver == config.FsDriverFscache {
		instances := d.Instances.List()
		for _, i := range instances {
			if err := mounter.Umount(i.GetMountpoint()); err != nil {
				log.L.Warnf("Can't umount %s, %v", d.States.Mountpoint, err)
			}
		}
	} else {
		log.L.Infof("Unmounting %s when clear vestige", d.HostMountpoint())
		if err := mounter.Umount(d.HostMountpoint()); err != nil {
			log.L.Warnf("Can't umount %s, %v", d.States.Mountpoint, err)
		}
	}

	// Nydusd judges if it should enter failover phrase by checking
	// if unix socket is existed and it can't be connected.
	if err := os.Remove(d.GetAPISock()); err != nil {
		log.L.Warnf("Can't delete residual unix socket %s, %v", d.GetAPISock(), err)
	}

	// `CheckStatus->ensureClient` only checks if socket file is existed when building http client.
	// But the socket file may be residual and will be cleared before starting a new nydusd.
	// So clear the client by assigning nil
	d.ResetClient()
}

// Instantiate a daemon object
func NewDaemon(opt ...NewDaemonOpt) (*Daemon, error) {
	d := &Daemon{}
	d.States.ID = newID()
	d.States.DaemonMode = config.DaemonModeDedicated
	d.Instances = rafsSet{instances: make(map[string]*Rafs)}

	for _, o := range opt {
		err := o(d)
		if err != nil {
			return nil, err
		}
	}

	return d, nil
}
