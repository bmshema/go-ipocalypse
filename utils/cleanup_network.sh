#!/bin/bash

# Check if script is run with sudo
if [ "$EUID" -ne 0 ]; then 
    echo "Please run this script with sudo"
    exit 1
fi

echo "=== Cleaning Up Network Configuration ==="

# Remove Docker network
echo "Removing Docker network 'ipocalypse_net'..."
docker network rm ipocalypse_net 2>/dev/null || true

# Remove host macvlan interface
echo "Removing host macvlan interface..."
if ip link show macvlan0 >/dev/null 2>&1; then
    # Remove any routes associated with macvlan0
    while ip route show dev macvlan0 2>/dev/null | grep -q .; do
        route=$(ip route show dev macvlan0 | head -n1 | awk '{print $1}')
        ip route del "$route" dev macvlan0 2>/dev/null || true
    done
    
    # Bring down and delete the interface
    ip link set macvlan0 down 2>/dev/null || true
    ip link delete macvlan0 2>/dev/null || true
fi

echo "=== Cleanup Complete ==="
echo "The following changes have been made:"
echo "  - Removed Docker network 'ipocalypse_net'"
echo "  - Removed macvlan0 interface and its routes" 