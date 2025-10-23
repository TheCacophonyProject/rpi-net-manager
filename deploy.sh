#!/bin/bash

# Deploy rpi-net-manager to a Raspberry Pi discovered via mDNS
# - Builds the binary for Linux arm64
# - Copies binary to /usr/bin
# - Installs systemd service, DBus policy, and dhcpcd hook from _release
# - Runs the bundled postinstall script to reload/enable/restart service

# Start the SSH agent and add the key
eval "$(ssh-agent -s)"
ssh_key_location="$HOME/.ssh/cacophony-pi"
ssh-add "$ssh_key_location"

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &> /dev/null && pwd)

# Discover Raspberry Pi services on the network
echo "Discovering Raspberry Pis with service _cacophonator-management._tcp..."
readarray -t services < <(avahi-browse -t -r _cacophonator-management._tcp | grep 'hostname' | awk '{print $3}' | sed 's/\[//' | sed 's/\]//' | sed 's/\.$/.local/')

if [ ${#services[@]} -eq 0 ]; then
    echo "No Raspberry Pi services found on the network."
    exit 1
fi

# Display the discovered services
echo "Found Raspberry Pi services:"
for i in "${!services[@]}"; do
    echo "$((i + 1))) ${services[i]}"
done

# Let the user select a service
read -p "Select a Raspberry Pi to deploy to (1-${#services[@]}): " selection
pi_address=${services[$((selection - 1))]}

if [ -z "$pi_address" ]; then
    echo "Invalid selection."
    exit 1
fi

echo "Selected Raspberry Pi at: $pi_address"

build_binary() {
    echo "Building rpi-net-manager for linux/arm64..."
    ( cd "$SCRIPT_DIR" && \
      GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
      go build -o "$SCRIPT_DIR/rpi-net-manager" ./cmd/rpi-net-manager )
    if [ $? -ne 0 ]; then
        echo "Error: build failed"
        return 1
    fi
}

while true; do
    # Build
    build_binary || break

    # Ensure target directories exist (service/dbus/hook paths)
    echo "Preparing target directories on device..."
    ssh -i "$ssh_key_location" "pi@$pi_address" "sudo mkdir -p /etc/systemd/system /etc/dbus-1/system.d /lib/dhcpcd/dhcpcd-hooks" || break

    # Stop service if present
    echo "Stopping rpi-net-manager.service if running..."
    ssh -i "$ssh_key_location" "pi@$pi_address" "sudo systemctl stop rpi-net-manager.service || true"

    # Copy artifacts to home first
    echo "Copying binary and release files..."
    scp -i "$ssh_key_location" \
        "$SCRIPT_DIR/rpi-net-manager" \
        "$SCRIPT_DIR/_release/rpi-net-manager.service" \
        "$SCRIPT_DIR/_release/org.cacophony.RPiNetManager.conf" \
        "$SCRIPT_DIR/_release/10-notify-rpi-net-manager" \
        "$SCRIPT_DIR/_release/postinstall.sh" \
        "pi@$pi_address:/home/pi/" || { echo "Error: SCP failed"; break; }

    # Move into place with correct names/permissions
    echo "Installing files on device..."
    ssh -i "$ssh_key_location" "pi@$pi_address" "\
        sudo mv /home/pi/rpi-net-manager /usr/bin/ && \
        sudo chmod 755 /usr/bin/rpi-net-manager && \
        sudo mv /home/pi/rpi-net-manager.service /etc/systemd/system/rpi-net-manager.service && \
        sudo chmod 644 /etc/systemd/system/rpi-net-manager.service && \
        sudo mv /home/pi/org.cacophony.RPiNetManager.conf /etc/dbus-1/system.d/org.cacophony.RPiNetManager.conf && \
        sudo chmod 644 /etc/dbus-1/system.d/org.cacophony.RPiNetManager.conf && \
        sudo mv /home/pi/10-notify-rpi-net-manager /lib/dhcpcd/dhcpcd-hooks/10-rpi-net-manager && \
        sudo chmod 755 /lib/dhcpcd/dhcpcd-hooks/10-rpi-net-manager && \
        sudo chmod +x /home/pi/postinstall.sh" || { echo "Error: remote install failed"; break; }

    # Run postinstall to daemon-reload/enable/restart
    echo "Running postinstall script..."
    ssh -t -i "$ssh_key_location" "pi@$pi_address" "sudo /bin/bash /home/pi/postinstall.sh" || { echo "Error: postinstall failed"; break; }

    # Stream logs from the service
    echo "Streaming logs from rpi-net-manager.service... (press Ctrl+C to stop)"
    ssh -t -i "$ssh_key_location" "pi@$pi_address" "sudo journalctl -fu rpi-net-manager.service"

    echo "Deployment completed. Press any key to deploy again or Ctrl+C to exit."
    read -n 1 -s || break
    echo # new line for readability
done

# Kill the SSH agent when done
eval "$(ssh-agent -k)"

