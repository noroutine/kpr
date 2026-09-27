# Contrib

This directory contains supplementary files and examples for deploying and running kpr.

## Contents

### systemd/

Systemd service files for running kpr as a system service on Linux.

- **kpr.service** - Main service unit file

#### Installation

```bash
# Copy the service file
sudo cp contrib/systemd/kpr.service /etc/systemd/system/

# Create service user (optional but recommended)
sudo useradd -r -s /bin/false -d /var/lib/kpr kpr
sudo mkdir -p /var/lib/kpr
sudo chown kpr:kpr /var/lib/kpr

# Reload systemd and enable service
sudo systemctl daemon-reload
sudo systemctl enable kpr
sudo systemctl start kpr

# Check status
sudo systemctl status kpr
```

#### Configuration

Edit the service file to customize:
- User/Group: Change `User=` and `Group=` if not using dedicated user
- ExecStart: Update paths or add CLI flags
- Environment: Add environment variables with `Environment=KEY=VALUE`

#### Logs

View logs with journalctl:
```bash
# Follow logs
sudo journalctl -u kpr -f

# Recent logs
sudo journalctl -u kpr -n 100

# Logs since boot
sudo journalctl -u kpr -b
```

## Future Additions

This directory can be extended with:
- Docker/Podman examples
- Kubernetes manifests
- Nginx/Caddy reverse proxy configs
- Monitoring/alerting configs (Prometheus, etc.)
- Example configuration files
- Helper scripts
