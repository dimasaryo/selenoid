package service

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"time"

	"strings"

	"github.com/aerokube/selenoid/config"
	"github.com/aerokube/selenoid/session"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const comma = ","

// Docker - docker container manager
type Docker struct {
	ServiceBase
	Environment
	session.Caps
	LogConfig *container.LogConfig
	Client    *client.Client
}

type portConfig struct {
	SeleniumPort nat.Port
	VNCPort      nat.Port
	PortBindings nat.PortMap
	ExposedPorts map[nat.Port]struct{}
}

// StartWithCancel - Starter interface implementation
func (d *Docker) StartWithCancel() (*StartedService, error) {
	portConfig, err := getPortConfig(d.Service, d.Caps, d.Environment)
	if err != nil {
		return nil, fmt.Errorf("configuring ports: %v", err)
	}
	selenium := portConfig.SeleniumPort
	vnc := portConfig.VNCPort
	ctx := context.Background()
	log.Printf("[%d] [CREATING_CONTAINER] [%s]\n", d.RequestId, d.Service.Image)
	hostConfig := container.HostConfig{
		Binds:        d.Service.Volumes,
		AutoRemove:   true,
		PortBindings: portConfig.PortBindings,
		LogConfig:    *d.LogConfig,
		NetworkMode:  container.NetworkMode(d.Network),
		Tmpfs:        d.Service.Tmpfs,
		ShmSize:      getShmSize(d.Service),
		Privileged:   true,
		Resources: container.Resources{
			Memory:   d.Memory,
			NanoCPUs: d.CPU,
		},
		ExtraHosts: getExtraHosts(d.Service, d.Caps),
	}
	if d.ApplicationContainers != "" {
		links := strings.Split(d.ApplicationContainers, comma)
		hostConfig.Links = links
	}
	container, err := d.Client.ContainerCreate(ctx,
		&container.Config{
			Hostname:     getContainerHostname(d.Caps),
			Image:        d.Service.Image.(string),
			Env:          getEnv(d.ServiceBase, d.Caps),
			ExposedPorts: portConfig.ExposedPorts,
		},
		&hostConfig,
		&network.NetworkingConfig{}, "")
	if err != nil {
		return nil, fmt.Errorf("create container: %v", err)
	}
	containerStartTime := time.Now()
	log.Printf("[%d] [STARTING_CONTAINER] [%s] [%s]\n", d.RequestId, d.Service.Image, container.ID)
	err = d.Client.ContainerStart(ctx, container.ID, types.ContainerStartOptions{})
	if err != nil {
		d.removeContainer(ctx, d.Client, container.ID)
		return nil, fmt.Errorf("start container: %v", err)
	}
	log.Printf("[%d] [CONTAINER_STARTED] [%s] [%s] [%v]\n", d.RequestId, d.Service.Image, container.ID, time.Since(containerStartTime))
	stat, err := d.Client.ContainerInspect(ctx, container.ID)
	if err != nil {
		d.removeContainer(ctx, d.Client, container.ID)
		return nil, fmt.Errorf("inspect container %s: %s", container.ID, err)
	}
	_, ok := stat.NetworkSettings.Ports[selenium]
	if !ok {
		d.removeContainer(ctx, d.Client, container.ID)
		return nil, fmt.Errorf("no bindings available for %v", selenium)
	}
	seleniumHostPort, vncHostPort := getHostPort(d.Environment, d.Service, d.Caps, stat, selenium, vnc)
	u := &url.URL{Scheme: "http", Host: seleniumHostPort, Path: d.Service.Path}
	serviceStartTime := time.Now()
	err = wait(u.String(), d.StartupTimeout)
	if err != nil {
		d.removeContainer(ctx, d.Client, container.ID)
		return nil, fmt.Errorf("wait: %v", err)
	}
	log.Printf("[%d] [SERVICE_STARTED] [%s] [%s] [%v]\n", d.RequestId, d.Service.Image, container.ID, time.Since(serviceStartTime))
	log.Printf("[%d] [PROXY_TO] [%s] [%s] [%s]\n", d.RequestId, d.Service.Image, container.ID, u.String())
	s := StartedService{
		Url:         u,
		ID:          container.ID,
		VNCHostPort: vncHostPort,
		Cancel:      func() { d.removeContainer(ctx, d.Client, container.ID) },
	}
	return &s, nil
}

func getPortConfig(service *config.Browser, caps session.Caps, env Environment) (*portConfig, error) {
	selenium, err := nat.NewPort("tcp", service.Port)
	if err != nil {
		return nil, fmt.Errorf("new selenium port: %v", err)
	}
	exposedPorts := map[nat.Port]struct{}{selenium: {}}
	var vnc nat.Port
	enableVNC, err := strconv.ParseBool(caps.VNC)
	if enableVNC {
		vnc, err = nat.NewPort("tcp", service.Vnc)
		if err != nil {
			return nil, fmt.Errorf("new vnc port: %v", err)
		}
		exposedPorts[vnc] = struct{}{}
	}
	portBindings := nat.PortMap{}
	if env.IP != "" || !env.InDocker {
		portBindings[selenium] = []nat.PortBinding{{HostIP: "0.0.0.0"}}
		if enableVNC {
			portBindings[vnc] = []nat.PortBinding{{HostIP: "0.0.0.0"}}
		}
	}
	return &portConfig{
		SeleniumPort: selenium,
		VNCPort:      vnc,
		PortBindings: portBindings,
		ExposedPorts: exposedPorts}, nil
}

func getTimeZone(service ServiceBase, caps session.Caps) *time.Location {
	timeZone := time.Local
	if caps.TimeZone != "" {
		tz, err := time.LoadLocation(caps.TimeZone)
		if err != nil {
			log.Printf("[%d] [BAD_TIMEZONE] [%s]\n", service.RequestId, caps.TimeZone)
		} else {
			timeZone = tz
		}
	}
	return timeZone
}

func getEnv(service ServiceBase, caps session.Caps) []string {
	env := []string{
		fmt.Sprintf("TZ=%s", getTimeZone(service, caps)),
		fmt.Sprintf("SCREEN_RESOLUTION=%s", caps.ScreenResolution),
		fmt.Sprintf("ENABLE_VNC=%s", caps.VNC),
	}
	env = append(env, service.Service.Env...)
	return env
}

func getShmSize(service *config.Browser) int64 {
	if service.ShmSize > 0 {
		return service.ShmSize
	}
	return int64(268435456)
}

func getContainerHostname(caps session.Caps) string {
	if caps.ContainerHostname != "" {
		return caps.ContainerHostname
	}
	return "localhost"
}

func getExtraHosts(service *config.Browser, caps session.Caps) []string {
	extraHosts := service.Hosts
	if caps.HostsEntries != "" {
		extraHosts = append(strings.Split(caps.HostsEntries, comma), extraHosts...)
	}
	return extraHosts
}

func getHostPort(env Environment, service *config.Browser, caps session.Caps, stat types.ContainerJSON, selenium nat.Port, vnc nat.Port) (string, string) {
	seleniumHostPort, vncHostPort := "", ""
	enableVNC, _ := strconv.ParseBool(caps.VNC)
	if env.IP == "" {
		if env.InDocker {
			containerIP := getContainerIP(env.Network, stat)
			seleniumHostPort = net.JoinHostPort(containerIP, service.Port)
			if enableVNC {
				vncHostPort = net.JoinHostPort(containerIP, service.Vnc)
			}
		} else {
			seleniumHostPort = net.JoinHostPort("127.0.0.1", stat.NetworkSettings.Ports[selenium][0].HostPort)
			if enableVNC {
				vncHostPort = net.JoinHostPort("127.0.0.1", stat.NetworkSettings.Ports[vnc][0].HostPort)
			}
		}
	} else {
		seleniumHostPort = net.JoinHostPort(env.IP, stat.NetworkSettings.Ports[selenium][0].HostPort)
		if enableVNC {
			vncHostPort = net.JoinHostPort(env.IP, stat.NetworkSettings.Ports[vnc][0].HostPort)
		}
	}
	return seleniumHostPort, vncHostPort
}

func getContainerIP(networkName string, stat types.ContainerJSON) string {
	ns := stat.NetworkSettings
	if ns.IPAddress != "" {
		return stat.NetworkSettings.IPAddress
	}
	if len(ns.Networks) > 0 {
		possibleAddresses := []string{}
		for name, nt := range ns.Networks {
			if nt.IPAddress != "" {
				if name == networkName {
					return nt.IPAddress
				}
				possibleAddresses = append(possibleAddresses, nt.IPAddress)
			}
		}
		if len(possibleAddresses) > 0 {
			return possibleAddresses[0]
		}
	}
	return ""
}

func (d *Docker) removeContainer(ctx context.Context, cli *client.Client, id string) {
	log.Printf("[%d] [REMOVE_CONTAINER] [%s]\n", d.RequestId, id)
	err := cli.ContainerRemove(ctx, id, types.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil {
		log.Printf("[%d] [FAILED_TO_REMOVE_CONTAINER] [%s] [%v]\n", d.RequestId, id, err)
		return
	}
	log.Printf("[%d] [CONTAINER_REMOVED] [%s]\n", d.RequestId, id)
}
