#!/bin/bash

# Parse command line arguments
ENABLE_INTERNET=false
while getopts "i" opt; do
    case $opt in
        i)
            ENABLE_INTERNET=true
            ;;
    esac
done

# Check if script is run with sudo
if [ "$EUID" -ne 0 ]; then 
    echo "Please run this script with sudo"
    exit 1
fi

# Check if ipcalc is installed
if ! command -v ipcalc &> /dev/null; then
    echo "ipcalc is not installed. Installing..."
    apt-get update && apt-get install -y ipcalc
fi

echo "=== Detecting Network Configuration ==="
# Detect the primary network interface
iface=$(ip route show default 2>/dev/null | grep -Eo 'dev [^ ]+' | awk '{print $2}' | head -n1)

# Fallback if no default interface is found
if [ -z "$iface" ]; then
    echo "[WARN] No default interface found from routes. Trying to detect a likely interface..."
    fallback_iface=$(ip -o link show up | grep -oE '^[0-9]+: (en[^:]*|ens[^:]*|eno[^:]*|eth[^:]*)' | awk '{print $2}' | head -n1)
    iface="${fallback_iface:-eth0}"
fi

echo "Detected interface: $iface"

# Get subnet and gateway information
ip_addr=$(ip -o -f inet addr show "$iface" | awk '{print $4}')
IFS='/' read -r ip_address netmask <<< "$ip_addr"
network_addr=$(ipcalc "$ip_address/$netmask" | grep "Network:" | awk '{print $2}' | cut -d'/' -f1)
gateway=$(ip route show dev "$iface" | awk '/default/ {print $3}')

if [ -z "$network_addr" ] || [ -z "$gateway" ]; then
    echo "[ERROR] Could not detect network configuration"
    exit 1
fi

subnet="$network_addr/$netmask"
echo "Detected subnet: $subnet"
echo "Detected gateway: $gateway"

echo "=== Setting up Docker Network ==="
# Remove existing network if it exists
echo "Removing existing network if it exists..."
docker network rm ipocalypse_net 2>/dev/null

# Create the Docker network
echo "Creating new Docker network..."
docker network create \
    -d macvlan \
    -o parent=$iface \
    -o macvlan_mode=bridge \
    --subnet=$subnet \
    --gateway=$gateway \
    --attachable \
    ipocalypse_net

if [ $? -ne 0 ]; then
    echo "[ERROR] Failed to create Docker network"
    exit 1
fi

echo "=== Setting up Host Network Interface ==="
# Remove existing macvlan interface if it exists
ip link show macvlan0 >/dev/null 2>&1 && ip link delete macvlan0

# Create macvlan interface on the host
echo "Creating host macvlan interface..."
ip link add macvlan0 link $iface type macvlan mode bridge
ip addr add ${ip_address}/${netmask} dev macvlan0
ip link set macvlan0 up

# Add route for Docker network subnet
echo "Adding route for Docker containers..."
DOCKER_SUBNET=$(docker network inspect ipocalypse_net | grep Subnet | awk -F'"' '{print $4}')
ip route add $DOCKER_SUBNET dev macvlan0 2>/dev/null || true

# Configure NAT if internet access is enabled
if [ "$ENABLE_INTERNET" = true ]; then
    echo "Enabling internet access for containers..."
    # Enable IP forwarding
    sysctl -w net.ipv4.ip_forward=1 > /dev/null
    # Add NAT rule
    iptables -t nat -A POSTROUTING -s $subnet -j MASQUERADE
    echo "Internet access enabled"
fi

echo "=== Network Setup Complete ==="
echo "Docker network 'ipocalypse_net' created with:"
echo "  - Parent interface: $iface"
echo "  - Subnet: $subnet"
echo "  - Gateway: $gateway"
echo ""
echo "Host network interface configured:"
echo "  - Interface: macvlan0"
echo "  - IP: ${ip_address}/${netmask}"
echo "  - Docker subnet route: $DOCKER_SUBNET"
echo ""
echo "You can now run: docker-compose up -d" 