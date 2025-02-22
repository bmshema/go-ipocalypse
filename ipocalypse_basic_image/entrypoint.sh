#!/bin/bash

# Function to handle cleanup on exit
cleanup() {
    echo "Releasing DHCP lease..."
    dhclient -r $INTERFACE
    exit 0
}

# Trap termination signals to run cleanup
trap cleanup SIGINT SIGTERM

# Debug: Show all network interfaces at start
echo "=== Initial Network Interface Status ==="
ip link show
ip addr show
echo "======================================"

# Wait a moment for network interface to be ready
sleep 2

# Detect the network interface dynamically (strip @if* suffix)
INTERFACE=$(ip -o link show | awk -F': ' '/^[0-9]+: (eth|macvlan)[0-9]*@/ {gsub(/@.*/, "", $2); print $2; exit}')
if [ -z "$INTERFACE" ]; then
    # Fallback to interfaces without @if suffix
    INTERFACE=$(ip -o link show | awk -F': ' '/^[0-9]+: (eth|macvlan)[0-9]*[^@]/ {print $2; exit}')
fi

if [ -z "$INTERFACE" ]; then
    echo "No suitable network interface found!"
    echo "Available interfaces:"
    ip link show
    exit 1
fi

echo "Bringing up network interface: $INTERFACE"
ip link set $INTERFACE up

# Debug: Show interface status after bringing it up
echo "=== Interface Status After Up ==="
ip link show $INTERFACE
ip addr show $INTERFACE
echo "=============================="

# Try DHCP multiple times
max_attempts=3
attempt=1
while [ $attempt -le $max_attempts ]; do
    echo "Attempting DHCP lease on $INTERFACE (attempt $attempt of $max_attempts)..."
    
    # Debug: Show dhclient process status
    echo "Current dhclient processes:"
    ps aux | grep dhclient
    
    # Try to kill any existing dhclient processes
    pkill dhclient 2>/dev/null || true
    
    # Run dhclient with verbose output
    if dhclient -v $INTERFACE; then
        echo "DHCP lease obtained successfully!"
        echo "=== Network Status After DHCP ==="
        ip addr show $INTERFACE
        ip route show
        echo "==============================="
        break
    else
        echo "DHCP attempt $attempt failed"
        echo "=== Network Status After Failed DHCP Attempt ==="
        ip addr show $INTERFACE
        ip route show
        echo "==========================================="
        if [ $attempt -eq $max_attempts ]; then
            echo "All DHCP attempts failed!"
            exit 1
        fi
        sleep 5
    fi
    attempt=$((attempt + 1))
done

echo "Container setup complete. Running indefinitely..."
# Keep the container running and log any interface changes
while true; do
    echo "=== Current Network Status ==="
    ip addr show
    ip route show
    echo "============================"
    sleep 60
done
