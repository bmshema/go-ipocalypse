# go-ipocalypse
A tool for testing network behavior by deploying multiple containers with DHCP-assigned IP addresses in a shared network space.

## Overview

go-ipocalypse creates a macvlan network and launches containers that:
- Obtain IP addresses via DHCP
- Can be configured with or without internet access
- Run custom testing workloads
- Scale up until the IP address space is exhausted

## Prerequisites

- Linux host with Docker installed
- `sudo` access (required for network configuration)
- `ipcalc` package (will be automatically installed if missing)

## Installation
```bash
git clone https://github.com/yourusername/go-ipocalypse
cd go-ipocalypse
go build
```

## Usage

Basic usage:
```bash
sudo ./ipocalypse
```
### Command Options

- `-dockerfiles` **(optional)**: Comma-separated list of directories containing Dockerfiles. 
    - Must start with "ipocalypse". 
    - If not specified, automatically discovers all ipocalypse* directories.
- `-workers` **(default: 5)**: Number of concurrent container launch workers
- `-internet` **(default: false)**: Enable internet access for containers  
\
`ipocalypse_basic_image` has no functionality beyond connecting to the local network. Add your own custom Dockerfiles to additional directories to deploy containers with other workloads. Additional directories must start with `ipocalypse_` to be discovered. Example: `ipocalypse_<name_of_workload>`.

## Creating Custom Images

1. Create a new directory starting with "ipocalypse"
2. Add a Dockerfile and any required scripts
3. Ensure the container runs continuously and handles DHCP configuration

**Example basic image structure:**
```
ipocalypse_basic_image/
├── Dockerfile
└── entrypoint.sh
```

## Network Configuration

ipocalypse will:
1. Create a Docker macvlan network "ipocalypse_net"
2. Set up a host macvlan interface for container communication
3. Configure NAT if internet access is enabled
4. Launch containers using images from all ipocalypse* directories until ip addresses are exhausted

## Cleanup

To clean up the network configuration:
```bash
sudo utils/cleanup_network.sh
```

## Container DCHP Setup:
The `entrypoint.sh` for the ipocalypse_basic_image ensures each container properly joins the network and maintains its network connection by handling: 

**1. Network Setup:**
- Detects and configures the container's network interface
- Attempts to obtain an IP address via DHCP (tries up to 3 times)
- Handles cleanup of DHCP leases on container shutdown

**2. Monitoring:**
- Keeps the container running indefinitely
- Logs network status every 60 seconds
- Shows IP address and routing information

**3. Error Handling:**
- Provides detailed debugging output
- Gracefully handles failures in interface detection or DHCP
- Implements proper signal handling for clean container shutdown  

Configure functionality for other container images by copying this script to new ipocalypse_* Dockerfile directories and modifying the script as needed.
