package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
)

func main() {
	// Flags for Docker image building and container launching.
	var dockerfileDirs string
	var networkName string
	var workers int

	// Flags for network configuration.
	var subnet string
	var gateway string
	var parent string
	var autoNetwork bool
	var internetAccess bool

	flag.StringVar(&dockerfileDirs, "dockerfiles", "", "Comma-separated list of directories containing Dockerfiles")
	flag.StringVar(&networkName, "network", "ipocalypse_net", "Name for the macvlan Docker network")
	// If autoNetwork is true, these values will be auto-detected.
	flag.StringVar(&subnet, "subnet", "", "Subnet to use for the macvlan network (CIDR format)")
	flag.StringVar(&gateway, "gateway", "", "Gateway for the macvlan network")
	flag.StringVar(&parent, "parent", "", "Parent network interface on the host")
	flag.BoolVar(&autoNetwork, "auto-network", true, "Automatically detect network configuration (default route, subnet, gateway, interface)")
	flag.BoolVar(&internetAccess, "internet", false, "Configure containers with internet access (sets up host macvlan interface)")
	flag.IntVar(&workers, "workers", 5, "Number of concurrent container launch workers")
	flag.Parse()

	if dockerfileDirs == "" {
		fmt.Println("Provide at least one Dockerfile directory using the -dockerfiles flag")
		os.Exit(1)
	}
	dockerfileList := strings.Split(dockerfileDirs, ",")

	// If auto-detection is enabled, detect the network configuration.
	var hostIPWithCIDR string
	if autoNetwork {
		detectedIface, detectedIP, detectedSubnet, detectedGateway, err := detectNetworkConfig()
		if err != nil {
			fmt.Printf("[ERROR] Auto network detection failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("=== Detected Network Configuration ===")
		fmt.Printf("Detected interface: %s\n", detectedIface)
		fmt.Printf("Detected host IP: %s\n", detectedIP)
		fmt.Printf("Detected subnet: %s\n", detectedSubnet)
		fmt.Printf("Detected gateway: %s\n", detectedGateway)

		// Override flags.
		parent = detectedIface
		subnet = detectedSubnet
		gateway = detectedGateway
		hostIPWithCIDR = detectedIP
	} else {
		// If not auto-detecting, ensure that mandatory flags are provided.
		if parent == "" || subnet == "" || gateway == "" {
			fmt.Println("[ERROR] When -auto-network is false, you must provide -parent, -subnet, and -gateway")
			os.Exit(1)
		}
		// In manual mode, try to get the host IP on the given parent interface.
		ip, err := getInterfaceIP(parent)
		if err != nil {
			fmt.Printf("[ERROR] Failed to get IP address for interface %s: %v\n", parent, err)
			os.Exit(1)
		}
		hostIPWithCIDR = ip
	}

	// Create a Docker client.
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Printf("[ERROR] Error creating Docker client: %v\n", err)
		os.Exit(1)
	}

	// Build images from each provided Dockerfile directory.
	imageNames := make([]string, 0, len(dockerfileList))
	for i, dir := range dockerfileList {
		imageName := fmt.Sprintf("ipocalypse_%d:latest", i)
		fmt.Printf("Building image %s from directory %s\n", imageName, dir)
		if err = buildImage(cli, dir, imageName); err != nil {
			fmt.Printf("[ERROR] Building image from %s failed: %v\n", dir, err)
			os.Exit(1)
		}
		imageNames = append(imageNames, imageName)
	}

	// Ensure the Docker macvlan network exists.
	fmt.Printf("=== Ensuring Docker macvlan network '%s' exists ===\n", networkName)
	_, err = ensureMacvlanNetwork(cli, networkName, subnet, gateway, parent)
	if err != nil {
		fmt.Printf("[ERROR] Failed to create Docker network: %v\n", err)
		os.Exit(1)
	}

	// If internet access is desired, set up a host macvlan interface.
	if internetAccess {
		fmt.Println("=== Setting up host macvlan interface for internet access ===")
		err = setupHostMacvlanInterface(parent, hostIPWithCIDR, subnet)
		if err != nil {
			fmt.Printf("[ERROR] Failed to set up host macvlan interface: %v\n", err)
			os.Exit(1)
		}
	}

	// Start concurrent workers to launch containers.
	fmt.Println("=== Starting container launch workers ===")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errorChan := make(chan error, 1)

	// Seed the random number generator.
	rand.Seed(time.Now().UnixNano())

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					// Randomly select one of the built images.
					chosenImage := imageNames[rand.Intn(len(imageNames))]
					containerID, err := launchContainer(cli, chosenImage, networkName)
					if err != nil {
						fmt.Printf("[Worker %d] Error launching container: %v\n", workerID, err)
						// If error indicates that no IP was assigned, assume subnet exhaustion.
						if isNoIPError(err) {
							errorChan <- err
							cancel()
							return
						}
						// Otherwise, wait briefly and try again.
						time.Sleep(2 * time.Second)
						continue
					}
					fmt.Printf("[Worker %d] Launched container %s using image %s\n", workerID, containerID, chosenImage)
					time.Sleep(1 * time.Second)
				}
			}
		}(i)
	}

	// Wait until a worker signals an error (e.g. no IP available) or cancellation.
	select {
	case err := <-errorChan:
		fmt.Printf("Stopping container launches due to error: %v\n", err)
		cancel()
	case <-ctx.Done():
	}

	wg.Wait()
	fmt.Println("Finished launching containers.")
}

// buildImage builds a Docker image from the specified directory (which must contain a Dockerfile)
// and tags it with the provided imageName.
func buildImage(cli *client.Client, dockerfileDir, imageName string) error {
	ctx := context.Background()
	// Create a tar archive of the Dockerfile directory.
	buildContext, err := archive.TarWithOptions(dockerfileDir, &archive.TarOptions{})
	if err != nil {
		return err
	}
	buildOptions := types.ImageBuildOptions{
		Tags:       []string{imageName},
		Dockerfile: "Dockerfile",
		Remove:     true,
	}
	response, err := cli.ImageBuild(ctx, buildContext, buildOptions)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, err = io.Copy(os.Stdout, response.Body)
	return err
}

// ensureMacvlanNetwork creates (if needed) and returns a Docker network with the macvlan driver.
// It now sets the macvlan mode to "bridge" and marks the network as attachable.
func ensureMacvlanNetwork(cli *client.Client, networkName, subnet, gateway, parent string) (string, error) {
	ctx := context.Background()
	// Check if the network already exists.
	filterArgs := filters.NewArgs()
	filterArgs.Add("name", networkName)
	networks, err := cli.NetworkList(ctx, types.NetworkListOptions{Filters: filterArgs})
	if err != nil {
		return "", err
	}
	if len(networks) > 0 {
		return networks[0].ID, nil
	}
	// Configure IPAM.
	ipamConfig := &network.IPAM{
		Driver: "default",
		Config: []network.IPAMConfig{
			{
				Subnet:  subnet,
				Gateway: gateway,
			},
		},
	}
	netCreate := types.NetworkCreate{
		Driver: "macvlan",
		IPAM:   ipamConfig,
		Options: map[string]string{
			"parent":       parent,
			"macvlan_mode": "bridge",
			"attachable":   "true",
		},
	}
	response, err := cli.NetworkCreate(ctx, networkName, netCreate)
	if err != nil {
		return "", err
	}
	return response.ID, nil
}

// launchContainer creates and starts a container using the given image and attaches it to the specified network.
// The container's command starts a DHCP client (assuming "dhclient" is installed) on its eth0 interface and then sleeps.
func launchContainer(cli *client.Client, imageName, networkName string) (string, error) {
	ctx := context.Background()
	containerConfig := &container.Config{
		Image: imageName,
		// The command runs dhclient on eth0 and then sleeps (adjust as needed).
		Cmd: []string{"sh", "-c", "dhclient eth0 && sleep 3600"},
	}
	hostConfig := &container.HostConfig{}
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			networkName: {},
		},
	}
	resp, err := cli.ContainerCreate(ctx, containerConfig, hostConfig, networkingConfig, nil, "")
	if err != nil {
		return "", err
	}
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return resp.ID, err
	}
	// Wait a short period to allow DHCP to assign an IP.
	time.Sleep(10 * time.Second)
	inspect, err := cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		return resp.ID, err
	}
	ep, ok := inspect.NetworkSettings.Networks[networkName]
	if !ok || ep.IPAddress == "" {
		// Remove the container if no IP was assigned.
		cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return resp.ID, fmt.Errorf("container did not receive an IP address")
	}
	return resp.ID, nil
}

// isNoIPError returns true if the error message indicates that no IP address was assigned.
func isNoIPError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "did not receive an IP address")
}

// detectNetworkConfig auto-detects the default network configuration by invoking Linux commands.
// It returns the default interface name, its IP address (in CIDR notation), the computed subnet, and the gateway.
func detectNetworkConfig() (iface, ipWithCIDR, subnet, gateway string, err error) {
	// Get default route information.
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to run 'ip route show default': %v", err)
	}
	routeLine := strings.TrimSpace(string(out))
	if routeLine == "" {
		// Fallback: try to detect a likely interface.
		iface, err = detectFallbackInterface()
		if err != nil {
			return "", "", "", "", fmt.Errorf("no default route found and fallback detection failed: %v", err)
		}
	} else {
		// Expect a line like: "default via 192.168.1.1 dev eth0 ..."
		parts := strings.Fields(routeLine)
		for i, part := range parts {
			if part == "dev" && i+1 < len(parts) {
				iface = parts[i+1]
			}
			if part == "via" && i+1 < len(parts) {
				gateway = parts[i+1]
			}
		}
		if iface == "" || gateway == "" {
			return "", "", "", "", fmt.Errorf("could not parse default route: %s", routeLine)
		}
	}

	// Get the IP address on the default interface.
	ipWithCIDR, err = getInterfaceIP(iface)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to get IP address for interface %s: %v", iface, err)
	}

	// Parse the CIDR and compute the network.
	_, ipNet, err := net.ParseCIDR(ipWithCIDR)
	if err != nil {
		return "", "", "", "", fmt.Errorf("failed to parse CIDR %s: %v", ipWithCIDR, err)
	}
	subnet = ipNet.String()
	return iface, ipWithCIDR, subnet, gateway, nil
}

// getInterfaceIP returns the first IPv4 address (in CIDR notation) for the given interface.
func getInterfaceIP(iface string) (string, error) {
	out, err := exec.Command("ip", "-o", "-f", "inet", "addr", "show", iface).Output()
	if err != nil {
		return "", err
	}
	// Expected output example:
	// "2: eth0    inet 192.168.1.100/24 brd 192.168.1.255 scope global dynamic eth0"
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Look for the field that contains a '/' (CIDR notation).
		for _, field := range fields {
			if strings.Contains(field, "/") {
				return field, nil
			}
		}
	}
	return "", fmt.Errorf("no IPv4 address found on interface %s", iface)
}

// detectFallbackInterface attempts to pick a likely interface from those that are "up".
func detectFallbackInterface() (string, error) {
	out, err := exec.Command("ip", "-o", "link", "show", "up").Output()
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		// Typical interface names: en*, ens*, eno*, eth*
		if strings.Contains(line, "en") || strings.Contains(line, "eth") {
			// The format is like: "2: eth0: <BROADCAST,..."
			parts := strings.Split(line, ":")
			if len(parts) >= 2 {
				iface := strings.TrimSpace(parts[1])
				// Remove any trailing characters (like '@')
				if i := strings.Index(iface, "@"); i != -1 {
					iface = iface[:i]
				}
				return iface, nil
			}
		}
	}
	// Fallback default.
	return "eth0", nil
}

// setupHostMacvlanInterface configures a host macvlan interface (named "macvlan0")
// so that containers on the Docker macvlan network can access the external network.
// This function mimics the behavior of the provided bash script.
func setupHostMacvlanInterface(parent, ipWithCIDR, dockerSubnet string) error {
	// Remove existing macvlan0 interface if it exists.
	_ = exec.Command("ip", "link", "show", "macvlan0").Run()
	_ = exec.Command("ip", "link", "delete", "macvlan0").Run()

	// Create the macvlan0 interface.
	fmt.Println("Creating host macvlan interface 'macvlan0' ...")
	if err := exec.Command("ip", "link", "add", "macvlan0", "link", parent, "type", "macvlan", "mode", "bridge").Run(); err != nil {
		return fmt.Errorf("failed to add macvlan0 interface: %v", err)
	}

	// Assign the host IP (with CIDR) to macvlan0.
	if err := exec.Command("ip", "addr", "add", ipWithCIDR, "dev", "macvlan0").Run(); err != nil {
		return fmt.Errorf("failed to assign IP address to macvlan0: %v", err)
	}

	// Bring macvlan0 up.
	if err := exec.Command("ip", "link", "set", "macvlan0", "up").Run(); err != nil {
		return fmt.Errorf("failed to set macvlan0 up: %v", err)
	}

	// Add a route for the Docker network subnet.
	if err := exec.Command("ip", "route", "add", dockerSubnet, "dev", "macvlan0").Run(); err != nil {
		// It is acceptable if the route already exists.
		fmt.Printf("Warning: failed to add route (%v)\n", err)
	}

	fmt.Println("Host macvlan interface configured successfully.")
	return nil
}
