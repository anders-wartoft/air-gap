# Docker Testing Environment

Simple Docker-based testing for air-gap packages.

## Quick Start

```bash
# From project root
cd tests

# Start containers
docker-compose up -d

# Test RPM on Rocky Linux
docker-compose exec rocky bash /scripts/test-rpm.sh

# Test DEB on Ubuntu
docker-compose exec ubuntu bash /scripts/test-deb.sh

# Test APK on Alpine
docker-compose exec alpine sh /scripts/test-apk.sh

# Stop containers
docker-compose down
```

## Available Containers

- **rocky** - Rocky Linux 9 (RPM packages)
- **ubuntu** - Ubuntu 22.04 (DEB packages)
- **alpine** - Alpine 3.18 (APK packages)

## Manual Testing

```bash
# Enter a container
docker-compose exec rocky bash

# Packages are mounted at /packages
ls /packages

# Install manually
dnf install -y /packages/airgap-upstream-*.rpm

# Test binary
/usr/local/bin/airgap-upstream --help
```

## Prerequisites

- Docker and Docker Compose installed
- Built packages in `../../target/dist/`
- Run `make package-all` first

## Notes

- Packages are mounted read-only
- Containers run indefinitely (`tail -f /dev/null`)
- Test scripts in `./scripts/` are executable
- No Kafka required for package testing

## Future Additions

- Add Kafka container for full integration tests
- Add multi-node deployment testing
- Add Vagrant configurations (coming soon)
