package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/archive"
)

func main() {
	// Flags for Docker image building and container launching
	var dockerfileDirs string
	var workers int
	var enableInternet bool

	flag.StringVar(&dockerfileDirs, "dockerfiles", "", "Comma-separated list of directories containing Dockerfiles")
	flag.IntVar(&workers, "workers", 5, "Number of concurrent container launch workers")
	flag.BoolVar(&enableInternet, "internet", false, "Enable internet access for containers")
	flag.Parse()

	if dockerfileDirs == "" {
		fmt.Println("Provide at least one Dockerfile directory using the -dockerfiles flag")
		os.Exit(1)
	}

	// Execute setup_network.sh with internet flag if enabled
	fmt.Println("Setting up network configuration...")
	var setupCmd *exec.Cmd
	if enableInternet {
		setupCmd = exec.Command("sudo", "./setup_network.sh", "-i")
		fmt.Println("Internet access enabled for containers")
	} else {
		setupCmd = exec.Command("sudo", "./setup_network.sh")
		fmt.Println("Internet access disabled for containers")
	}
	setupCmd.Stdout = os.Stdout
	setupCmd.Stderr = os.Stderr
	if err := setupCmd.Run(); err != nil {
		fmt.Printf("Failed to setup network: %v\n", err)
		os.Exit(1)
	}

	// Setup cleanup on interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nCleaning up network configuration...")
		cleanupCmd := exec.Command("sudo", "./cleanup_network.sh")
		cleanupCmd.Stdout = os.Stdout
		cleanupCmd.Stderr = os.Stderr
		if err := cleanupCmd.Run(); err != nil {
			fmt.Printf("Failed to cleanup network: %v\n", err)
		}
		os.Exit(0)
	}()

	// Continue with your existing container setup logic...
	dockerfileList := strings.Split(dockerfileDirs, ",")
	fmt.Printf("Processing %d Dockerfile directories with %d workers\n", len(dockerfileList), workers)

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
					containerID, err := launchContainer(cli, chosenImage)
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

	select {} // Keep the program running
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

// launchContainer creates and starts a container using the given image and attaches it to the specified network.
// The container's command starts a DHCP client (assuming "dhclient" is installed) on its eth0 interface and then sleeps.
func launchContainer(cli *client.Client, imageName string) (string, error) {
	ctx := context.Background()
	containerConfig := &container.Config{
		Image: imageName,
		Cmd:   []string{"sh", "-c", "dhclient eth0 && sleep 3600"},
	}
	hostConfig := &container.HostConfig{}

	// Specify the network configuration
	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			"ipocalypse_net": {
				NetworkID: "ipocalypse_net",
			},
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
	ep, ok := inspect.NetworkSettings.Networks["ipocalypse_net"]
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

func setupHostMacvlanInterface(parent, ipWithCIDR, dockerSubnet string) error {
	// Remove existing macvlan0 interface if it exists
	if err := exec.Command("bash", "-c", "ip link show macvlan0").Run(); err == nil {
		if err := exec.Command("bash", "-c", "ip link delete macvlan0").Run(); err != nil {
			return fmt.Errorf("failed to delete existing macvlan0: %v", err)
		}
	}

	// Create the macvlan interface exactly as in setup_network.sh
	createCmd := fmt.Sprintf("ip link add macvlan0 link %s type macvlan mode bridge", parent)
	if err := exec.Command("bash", "-c", createCmd).Run(); err != nil {
		return fmt.Errorf("failed to create macvlan0 interface: %v", err)
	}

	// Add IP address to macvlan0
	addrCmd := fmt.Sprintf("ip addr add %s dev macvlan0", ipWithCIDR)
	if err := exec.Command("bash", "-c", addrCmd).Run(); err != nil {
		return fmt.Errorf("failed to assign IP address to macvlan0: %v", err)
	}

	// Bring up the interface
	if err := exec.Command("bash", "-c", "ip link set macvlan0 up").Run(); err != nil {
		return fmt.Errorf("failed to bring up macvlan0: %v", err)
	}

	// Add route for Docker subnet
	routeCmd := fmt.Sprintf("ip route add %s dev macvlan0", dockerSubnet)
	if err := exec.Command("bash", "-c", routeCmd).Run(); err != nil {
		fmt.Printf("Warning: failed to add route (%v)\n", err)
	}

	return nil
}
