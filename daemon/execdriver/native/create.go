// +build linux,cgo

package native

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/libcontainer/apparmor"
	"github.com/docker/libcontainer/configs"
	"github.com/docker/libcontainer/devices"
	"github.com/docker/libcontainer/utils"
)

// createContainer populates and configures the container type with the
// data provided by the execdriver.Command
func (d *driver) createContainer(c *execdriver.Command) (*configs.Config, error) {
	container := execdriver.InitContainer(c)

	if err := d.createIpc(container, c); err != nil {
		return nil, err
	}

	if err := d.createPid(container, c); err != nil {
		return nil, err
	}

	if err := d.createUTS(container, c); err != nil {
		return nil, err
	}

	if err := d.createNetwork(container, c); err != nil {
		return nil, err
	}

	if c.ProcessConfig.Privileged {
		// clear readonly for /sys
		for i := range container.Mounts {
			if container.Mounts[i].Destination == "/sys" {
				container.Mounts[i].Flags &= ^syscall.MS_RDONLY
			}
		}
		container.ReadonlyPaths = nil
		container.MaskPaths = nil
		if err := d.setPrivileged(container); err != nil {
			return nil, err
		}
	} else {
		if err := d.setCapabilities(container, c); err != nil {
			return nil, err
		}
	}

	if c.AppArmorProfile != "" {
		container.AppArmorProfile = c.AppArmorProfile
	}

	if err := execdriver.SetupCgroups(container, c); err != nil {
		return nil, err
	}

	if err := d.setupMounts(container, c); err != nil {
		return nil, err
	}

	d.setupLabels(container, c)
	d.setupRlimits(container, c)
	return container, nil
}

func generateIfaceName() (string, error) {
	for i := 0; i < 10; i++ {
		name, err := utils.GenerateRandomName("veth", 7)
		if err != nil {
			continue
		}
		if _, err := net.InterfaceByName(name); err != nil {
			if strings.Contains(err.Error(), "no such") {
				return name, nil
			}
			return "", err
		}
	}
	return "", errors.New("Failed to find name for new interface")
}

func (d *driver) createNetwork(container *configs.Config, c *execdriver.Command) error {
	if c.Network.HostNetworking {
		container.Namespaces.Remove(configs.NEWNET)
		return nil
	}

	container.Networks = []*configs.Network{
		{
			Type: "loopback",
		},
	}

	iName, err := generateIfaceName()
	if err != nil {
		return err
	}
	if c.Network.Interface != nil {
		vethNetwork := configs.Network{
			Name:              "eth0",
			HostInterfaceName: iName,
			Mtu:               c.Network.Mtu,
			Address:           fmt.Sprintf("%s/%d", c.Network.Interface.IPAddress, c.Network.Interface.IPPrefixLen),
			MacAddress:        c.Network.Interface.MacAddress,
			Gateway:           c.Network.Interface.Gateway,
			Type:              "veth",
			Bridge:            c.Network.Interface.Bridge,
			HairpinMode:       c.Network.Interface.HairpinMode,
		}
		if c.Network.Interface.GlobalIPv6Address != "" {
			vethNetwork.IPv6Address = fmt.Sprintf("%s/%d", c.Network.Interface.GlobalIPv6Address, c.Network.Interface.GlobalIPv6PrefixLen)
			vethNetwork.IPv6Gateway = c.Network.Interface.IPv6Gateway
		}
		container.Networks = append(container.Networks, &vethNetwork)
	}

	if c.Network.ContainerID != "" {
		d.Lock()
		active := d.activeContainers[c.Network.ContainerID]
		d.Unlock()

		if active == nil {
			return fmt.Errorf("%s is not a valid running container to join", c.Network.ContainerID)
		}

		state, err := active.State()
		if err != nil {
			return err
		}

		container.Namespaces.Add(configs.NEWNET, state.NamespacePaths[configs.NEWNET])
	}

	return nil
}

func (d *driver) createIpc(container *configs.Config, c *execdriver.Command) error {
	if c.Ipc.HostIpc {
		container.Namespaces.Remove(configs.NEWIPC)
		return nil
	}

	if c.Ipc.ContainerID != "" {
		d.Lock()
		active := d.activeContainers[c.Ipc.ContainerID]
		d.Unlock()

		if active == nil {
			return fmt.Errorf("%s is not a valid running container to join", c.Ipc.ContainerID)
		}

		state, err := active.State()
		if err != nil {
			return err
		}
		container.Namespaces.Add(configs.NEWIPC, state.NamespacePaths[configs.NEWIPC])
	}

	return nil
}

func (d *driver) createPid(container *configs.Config, c *execdriver.Command) error {
	if c.Pid.HostPid {
		container.Namespaces.Remove(configs.NEWPID)
		return nil
	}

	return nil
}

func (d *driver) createUTS(container *configs.Config, c *execdriver.Command) error {
	if c.UTS.HostUTS {
		container.Namespaces.Remove(configs.NEWUTS)
		container.Hostname = ""
		return nil
	}

	return nil
}

func (d *driver) setPrivileged(container *configs.Config) (err error) {
	container.Capabilities = execdriver.GetAllCapabilities()
	container.Cgroups.AllowAllDevices = true

	hostDevices, err := devices.HostDevices()
	if err != nil {
		return err
	}
	container.Devices = hostDevices

	if apparmor.IsEnabled() {
		container.AppArmorProfile = "unconfined"
	}

	return nil
}

func (d *driver) setCapabilities(container *configs.Config, c *execdriver.Command) (err error) {
	container.Capabilities, err = execdriver.TweakCapabilities(container.Capabilities, c.CapAdd, c.CapDrop)
	return err
}

func (d *driver) setupRlimits(container *configs.Config, c *execdriver.Command) {
	if c.Resources == nil {
		return
	}

	for _, rlimit := range c.Resources.Rlimits {
		container.Rlimits = append(container.Rlimits, configs.Rlimit{
			Type: rlimit.Type,
			Hard: rlimit.Hard,
			Soft: rlimit.Soft,
		})
	}
}

func (d *driver) setupMounts(container *configs.Config, c *execdriver.Command) error {
	userMounts := make(map[string]struct{})
	for _, m := range c.Mounts {
		userMounts[m.Destination] = struct{}{}
	}

	// Filter out mounts that are overriden by user supplied mounts
	var defaultMounts []*configs.Mount
	_, mountDev := userMounts["/dev"]
	for _, m := range container.Mounts {
		if _, ok := userMounts[m.Destination]; !ok {
			if mountDev && strings.HasPrefix(m.Destination, "/dev/") {
				continue
			}
			defaultMounts = append(defaultMounts, m)
		}
	}
	container.Mounts = defaultMounts

	for _, m := range c.Mounts {
		flags := syscall.MS_BIND | syscall.MS_REC
		if !m.Writable {
			flags |= syscall.MS_RDONLY
		}
		if m.Slave {
			flags |= syscall.MS_SLAVE
		}
		container.Mounts = append(container.Mounts, &configs.Mount{
			Source:      m.Source,
			Destination: m.Destination,
			Device:      "bind",
			Flags:       flags,
		})
	}
	return nil
}

func (d *driver) setupLabels(container *configs.Config, c *execdriver.Command) {
	container.ProcessLabel = c.ProcessLabel
	container.MountLabel = c.MountLabel
}
