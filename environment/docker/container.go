package environment

import (
	"bufio"
	"context"
	"fmt"
	"github.com/apex/log"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/pkg/errors"
	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/environment"
	"github.com/pterodactyl/wings/system"
	"io"
	"strconv"
	"strings"
	"time"
)

// Attaches to the docker container itself and ensures that we can pipe data in and out
// of the process stream. This should not be used for reading console data as you *will*
// miss important output at the beginning because of the time delay with attaching to the
// output.
func (d *DockerEnvironment) Attach() error {
	if d.IsAttached() {
		return nil
	}

	if err := d.followOutput(); err != nil {
		return errors.WithStack(err)
	}

	opts := types.ContainerAttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	}

	// Set the stream again with the container.
	if st, err := d.client.ContainerAttach(context.Background(), d.Id, opts); err != nil {
		return errors.WithStack(err)
	} else {
		d.SetStream(&st)
	}

	console := new(Console)

	// TODO: resource polling should be handled by the server itself and just call a function
	//  on the environment that can return the data. Same for disabling polling.
	go func() {
		defer d.stream.Close()
		defer func() {
			d.setState(system.ProcessOfflineState)
			d.SetStream(nil)
		}()

		_, _ = io.Copy(console, d.stream.Reader)
	}()

	return nil
}

func (d *DockerEnvironment) resources() container.Resources {
	l := d.Configuration.Limits()

	return container.Resources{
		Memory:            l.BoundedMemoryLimit(),
		MemoryReservation: l.MemoryLimit * 1_000_000,
		MemorySwap:        l.ConvertedSwap(),
		CPUQuota:          l.ConvertedCpuLimit(),
		CPUPeriod:         100_000,
		CPUShares:         1024,
		BlkioWeight:       l.IoWeight,
		OomKillDisable:    l.OOMDisabled,
		CpusetCpus:        l.Threads,
	}
}

// Performs an in-place update of the Docker container's resource limits without actually
// making any changes to the operational state of the container. This allows memory, cpu,
// and IO limitations to be adjusted on the fly for individual instances.
func (d *DockerEnvironment) InSituUpdate() error {
	if _, err := d.client.ContainerInspect(context.Background(), d.Id); err != nil {
		// If the container doesn't exist for some reason there really isn't anything
		// we can do to fix that in this process (it doesn't make sense at least). In those
		// cases just return without doing anything since we still want to save the configuration
		// to the disk.
		//
		// We'll let a boot process make modifications to the container if needed at this point.
		if client.IsErrNotFound(err) {
			return nil
		}

		return errors.WithStack(err)
	}

	u := container.UpdateConfig{
		Resources: d.resources(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	if _, err := d.client.ContainerUpdate(ctx, d.Id, u); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Creates a new container for the server using all of the data that is currently
// available for it. If the container already exists it will be returned.
func (d *DockerEnvironment) Create(invocation string) error {
	// If the container already exists don't hit the user with an error, just return
	// the current information about it which is what we would do when creating the
	// container anyways.
	if _, err := d.client.ContainerInspect(context.Background(), d.Id); err == nil {
		return nil
	} else if !client.IsErrNotFound(err) {
		return errors.WithStack(err)
	}

	// Try to pull the requested image before creating the container.
	if err := d.ensureImageExists(d.image); err != nil {
		return errors.WithStack(err)
	}

	a := d.Configuration.Allocations()

	conf := &container.Config{
		Hostname:     d.Id,
		Domainname:   config.Get().Docker.Domainname,
		User:         strconv.Itoa(config.Get().System.User.Uid),
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		Tty:          true,
		ExposedPorts: a.Exposed(),
		Image:        d.image,
		Env:          d.Configuration.EnvironmentVariables(invocation),
		Labels: map[string]string{
			"Service":       "Pterodactyl",
			"ContainerType": "server_process",
		},
	}

	hostConf := &container.HostConfig{
		PortBindings: a.Bindings(),

		// Configure the mounts for this container. First mount the server data directory
		// into the container as a r/w bind.
		Mounts: d.convertMounts(),

		// Configure the /tmp folder mapping in containers. This is necessary for some
		// games that need to make use of it for downloads and other installation processes.
		Tmpfs: map[string]string{
			"/tmp": "rw,exec,nosuid,size=50M",
		},

		// Define resource limits for the container based on the data passed through
		// from the Panel.
		Resources: d.resources(),

		DNS: config.Get().Docker.Network.Dns,

		// Configure logging for the container to make it easier on the Daemon to grab
		// the server output. Ensure that we don't use too much space on the host machine
		// since we only need it for the last few hundred lines of output and don't care
		// about anything else in it.
		LogConfig: container.LogConfig{
			Type: jsonfilelog.Name,
			Config: map[string]string{
				"max-size": "5m",
				"max-file": "1",
			},
		},

		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		CapDrop: []string{
			"setpcap", "mknod", "audit_write", "net_raw", "dac_override",
			"fowner", "fsetid", "net_bind_service", "sys_chroot", "setfcap",
		},
		NetworkMode: container.NetworkMode(config.Get().Docker.Network.Mode),
	}

	if _, err := d.client.ContainerCreate(context.Background(), conf, hostConf, nil, d.Id); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (d *DockerEnvironment) convertMounts() []mount.Mount {
	var out []mount.Mount

	for _, m := range d.Configuration.Mounts() {
		out = append(out, mount.Mount{
			Type:          mount.TypeBind,
			Source:        m.Source,
			Target:        m.Target,
			ReadOnly:      m.ReadOnly,
		})
	}

	return out
}

// Remove the Docker container from the machine. If the container is currently running
// it will be forcibly stopped by Docker.
func (d *DockerEnvironment) Destroy() error {
	// We set it to stopping than offline to prevent crash detection from being triggered.
	d.setState(system.ProcessStoppingState)

	err := d.client.ContainerRemove(context.Background(), d.Id, types.ContainerRemoveOptions{
		RemoveVolumes: true,
		RemoveLinks:   false,
		Force:         true,
	})

	// Don't trigger a destroy failure if we try to delete a container that does not
	// exist on the system. We're just a step ahead of ourselves in that case.
	//
	// @see https://github.com/pterodactyl/panel/issues/2001
	if err != nil && client.IsErrNotFound(err) {
		return nil
	}

	d.setState(system.ProcessOfflineState)

	return err
}

// Attaches to the log for the container. This avoids us missing cruicial output that
// happens in the split seconds before the code moves from 'Starting' to 'Attaching'
// on the process.
func (d *DockerEnvironment) followOutput() error {
	if exists, err := d.Exists(); !exists {
		if err != nil {
			return errors.WithStack(err)
		}

		return errors.New(fmt.Sprintf("no such container: %s", d.Id))
	}

	opts := types.ContainerLogsOptions{
		ShowStderr: true,
		ShowStdout: true,
		Follow:     true,
		Since:      time.Now().Format(time.RFC3339),
	}

	reader, err := d.client.ContainerLogs(context.Background(), d.Id, opts)

	go func(r io.ReadCloser) {
		defer r.Close()

		s := bufio.NewScanner(r)
		for s.Scan() {
			d.Events().Publish(environment.ConsoleOutputEvent, s.Text())
		}

		if err := s.Err(); err != nil {
			log.WithField("error", err).WithField("container_id", d.Id).Warn("error processing scanner line in console output")
		}
	}(reader)

	return errors.WithStack(err)
}

// Pulls the image from Docker. If there is an error while pulling the image from the source
// but the image already exists locally, we will report that error to the logger but continue
// with the process.
//
// The reasoning behind this is that Quay has had some serious outages as of late, and we don't
// need to block all of the servers from booting just because of that. I'd imagine in a lot of
// cases an outage shouldn't affect users too badly. It'll at least keep existing servers working
// correctly if anything.
//
// TODO: handle authorization & local images
func (d *DockerEnvironment) ensureImageExists(image string) error {
	// Give it up to 15 minutes to pull the image. I think this should cover 99.8% of cases where an
	// image pull might fail. I can't imagine it will ever take more than 15 minutes to fully pull
	// an image. Let me know when I am inevitably wrong here...
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*15)
	defer cancel()

	// Get a registry auth configuration from the config.
	var registryAuth *config.RegistryConfiguration
	for registry, c := range config.Get().Docker.Registries {
		if !strings.HasPrefix(image, registry) {
			continue
		}

		log.WithField("registry", registry).Debug("using authentication for registry")
		registryAuth = &c
		break
	}

	// Get the ImagePullOptions.
	imagePullOptions := types.ImagePullOptions{All: false}
	if registryAuth != nil {
		b64, err := registryAuth.Base64()
		if err != nil {
			log.WithError(err).Error("failed to get registry auth credentials")
		}

		// b64 is a string so if there is an error it will just be empty, not nil.
		imagePullOptions.RegistryAuth = b64
	}

	out, err := d.client.ImagePull(ctx, image, imagePullOptions)
	if err != nil {
		images, ierr := d.client.ImageList(ctx, types.ImageListOptions{})
		if ierr != nil {
			// Well damn, something has gone really wrong here, just go ahead and abort there
			// isn't much anything we can do to try and self-recover from this.
			return ierr
		}

		for _, img := range images {
			for _, t := range img.RepoTags {
				if t != image {
					continue
				}

				log.WithFields(log.Fields{
					"image":        image,
					"container_id": d.Id,
					"error":        errors.New(err.Error()),
				}).Warn("unable to pull requested image from remote source, however the image exists locally")

				// Okay, we found a matching container image, in that case just go ahead and return
				// from this function, since there is nothing else we need to do here.
				return nil
			}
		}

		return err
	}
	defer out.Close()

	log.WithField("image", image).Debug("pulling docker image... this could take a bit of time")

	// I'm not sure what the best approach here is, but this will block execution until the image
	// is done being pulled, which is what we need.
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		continue
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}
