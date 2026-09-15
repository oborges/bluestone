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

### 1. Create Configuration File

Create a `config.yaml` file:

```yaml
server:
  nfs_port: 2049
  metrics_port: 8080
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

- [Full Documentation](../README.md)
- [Configuration Reference](CONFIGURATION.md)
- [Performance Tuning](PERFORMANCE.md)
- [Troubleshooting Guide](TROUBLESHOOTING.md)
- [API Documentation](API.md)

## Support

For issues and questions:
- GitHub Issues: https://github.com/oborges/bluestone/issues
- Documentation: https://github.com/oborges/bluestone/docs
