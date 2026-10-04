# Bluestone - Quick Start Guide

## Overview

Bluestone provides NFS filesystem access to IBM Cloud Object Storage (COS), enabling you to mount COS buckets as network filesystems on your IBM Cloud Virtual Server Instances (VSIs).

## Prerequisites

- IBM Cloud account with COS service
- IBM Cloud API key or HMAC credentials
- COS bucket created
- Docker, Kubernetes cluster, or a Linux host with systemd (for deployment)

## Quick Start with Linux systemd

### 1. Install the Service

```bash
git clone https://github.com/oborges/bluestone.git
cd bluestone
sudo ./scripts/install-linux-service.sh --build
```

### 2. Configure COS

```bash
sudoedit /etc/bluestone/config.yaml
```

Set your COS endpoint, bucket, region, and credentials. Secrets may also be
placed in `/etc/default/bluestone` as `BLUESTONE_` environment overrides.

### 3. Start the Service

```bash
sudo systemctl enable --now bluestone
sudo systemctl status bluestone
sudo journalctl -u bluestone -f
```

### 4. Mount the Export

```bash
sudo mkdir -p /mnt/cos
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos
```

For service hardening details, custom staging/cache paths, and upgrade notes,
see [Linux Service Installation](LINUX_SERVICE.md).

## Quick Start with Docker

No image is published: build one from a checkout first. The examples below
use the name and tag `make docker-build` gives it by default.

```bash
make docker-build VERSION=1.0.0
```

Podman works as well: `make docker-build VERSION=1.0.0 DOCKER=podman`, and
`podman` in place of `docker` below. With Podman, add `--format docker` to
the build if you want the image's health check kept.

### 1. Create Configuration File

Create a `config.yaml` file:

```yaml
server:
  nfs_port: 2049
  metrics_enabled: true # off by default; the checks under Monitoring need both
  metrics_port: 8080
  health_enabled: true
  health_port: 8081

cos:
  endpoint: "s3.us-south.cloud-object-storage.appdomain.cloud"
  bucket: "my-nfs-bucket"
  region: "us-south"
  auth_type: "iam"

cache:
  metadata:
    enabled: true
    size_mb: 256
    ttl_seconds: 60
  data:
    enabled: true
    size_gb: 10
    path: "/var/cache/bluestone"

logging:
  level: "info"
  format: "json"
```

### 2. Run with Docker

```bash
# Set your IBM Cloud API key
export IBM_CLOUD_API_KEY="your-api-key-here"

# Run the container
docker run -d \
  --name cos-bluestone \
  -p 2049:2049 \
  -p 8080:8080 \
  -p 8081:8081 \
  -e BLUESTONE_COS_API_KEY="${IBM_CLOUD_API_KEY}" \
  -v $(pwd)/config.yaml:/etc/bluestone/config.yaml \
  -v nfs-cache:/var/cache/bluestone \
  oborges/bluestone:1.0.0
```

### 3. Mount the NFS Share

On your client machine:

```bash
# Create mount point
sudo mkdir -p /mnt/cos

# Mount the NFS share
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 localhost:/ /mnt/cos

# Verify mount
df -h /mnt/cos
```

### 4. Test the Mount

```bash
# Create a test file
echo "Hello from COS!" > /mnt/cos/test.txt

# List files
ls -la /mnt/cos/

# Read the file
cat /mnt/cos/test.txt
```

## Quick Start with Docker Compose

### 1. Create docker-compose.yml

```yaml
version: '3.8'

services:
  bluestone:
    image: oborges/bluestone:1.0.0
    ports:
      - "2049:2049"
      - "8080:8080"
      - "8081:8081"
    environment:
      - BLUESTONE_COS_ENDPOINT=s3.us-south.cloud-object-storage.appdomain.cloud
      - BLUESTONE_COS_BUCKET=my-nfs-bucket
      - BLUESTONE_COS_REGION=us-south
      - BLUESTONE_COS_API_KEY=${IBM_CLOUD_API_KEY}
    volumes:
      - ./config.yaml:/etc/bluestone/config.yaml
      - nfs-cache:/var/cache/bluestone
    restart: unless-stopped

volumes:
  nfs-cache:
```

### 2. Start the Service

```bash
# Set your API key
export IBM_CLOUD_API_KEY="your-api-key-here"

# Start the service
docker-compose up -d

# Check logs
docker-compose logs -f
```

## Quick Start with Kubernetes

### 1. Create Secret

```bash
kubectl create secret generic bluestone-secret \
  --from-literal=ibm-cloud-api-key="your-api-key-here"
```

### 2. Deploy

The manifests name the image `oborges/bluestone:1.0.0`. Build it, push it to
a registry your cluster can pull from, and set `image:` in
`deployments/kubernetes/deployment.yaml` to match.

They run one gateway, as a bucket must have only one, with its staging area
on a PersistentVolumeClaim (`pvc.yaml`) so accepted writes survive a pod
restart. Only NFS is on the load balancer.

```bash
# Apply all manifests
kubectl apply -f deployments/kubernetes/

# Check status
kubectl get pods -l app=bluestone
kubectl get svc bluestone
```

### 3. Get Service IP

```bash
# Get the LoadBalancer IP
kubectl get svc bluestone -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

### 4. Mount from Client

```bash
# Replace <SERVICE_IP> with the actual IP
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049 <SERVICE_IP>:/ /mnt/cos
```

## Monitoring

The metrics and health servers are off by default and listen on 127.0.0.1
only. Turn them on with `server.metrics_enabled` and `server.health_enabled`.
In a container that means they are not reachable through a published port:
run the commands below inside it, for example
`docker exec cos-bluestone wget -qO- http://127.0.0.1:8081/health`. Use
`127.0.0.1` rather than `localhost` there, which resolves to `::1` first.
To reach them from outside, set `server.monitoring_address: "0.0.0.0"`;
neither server authenticates, so limit who can reach those ports. The
Kubernetes manifests do this, for the probes and the Prometheus scrape.

### Health Checks

```bash
# Liveness probe
curl http://localhost:8081/health/live

# Readiness probe
curl http://localhost:8081/health/ready

# Detailed health
curl http://localhost:8081/health
```

### Metrics

```bash
# View Prometheus metrics
curl http://localhost:8080/metrics
```

### Logs

```bash
# Docker
docker logs -f cos-bluestone

# Docker Compose
docker-compose logs -f

# Kubernetes
kubectl logs -f -l app=bluestone
```

## Configuration

### Environment Variables

All configuration can be overridden with environment variables using the `BLUESTONE_` prefix:

```bash
# Example
export BLUESTONE_LOGGING_LEVEL=debug
export BLUESTONE_CACHE_METADATA_ENABLED=true
export BLUESTONE_CACHE_DATA_SIZE_GB=20
```

### Authentication Methods

#### IAM (Recommended)

```yaml
cos:
  auth_type: "iam"
  api_key: "your-api-key"
```

#### HMAC

```yaml
cos:
  auth_type: "hmac"
  access_key: "your-access-key"
  secret_key: "your-secret-key"
```

## Performance Tuning

### For High Throughput

```yaml
performance:
  read_ahead_kb: 2048
  write_buffer_kb: 8192
  worker_pool_size: 200
  max_concurrent_reads: 100
  max_concurrent_writes: 50
  max_full_object_read_mb: 512
  max_buffered_write_mb: 512
  max_directory_entries: 100000

cache:
  data:
    size_gb: 50
```

### For Low Latency

```yaml
cache:
  metadata:
    ttl_seconds: 30
  data:
    enabled: true
    size_gb: 20

performance:
  read_ahead_kb: 512
```

## Troubleshooting

### Connection Issues

```bash
# Check if service is running
curl http://localhost:8081/health/live

# Check COS connectivity
curl http://localhost:8081/health/ready

# View logs
docker logs cos-bluestone
```

### Mount Issues

```bash
# Check NFS port is accessible
telnet localhost 2049

# Try mounting with verbose output
sudo mount -t nfs4 -o vers=4.0,tcp,port=2049,v localhost:/ /mnt/cos

# Check mount options
mount | grep /mnt/cos
```

### Performance Issues

```bash
# Check cache statistics
curl http://localhost:8080/metrics | grep cache

# Check COS API latency
curl http://localhost:8080/metrics | grep cos_api_duration

# Increase cache size in config.yaml
```

## Next Steps

- [Full documentation and configuration reference](../README.md)
- [Linux service installation](LINUX_SERVICE.md)
- [High availability](HA.md)
- [IBM Power: Linux and AIX](POWER.md)
- [Architecture](../ARCHITECTURE.md)

## Support

For issues and questions:
- GitHub Issues: https://github.com/oborges/bluestone/issues
- Documentation: https://github.com/oborges/bluestone/tree/main/docs
