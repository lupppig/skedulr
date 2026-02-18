#!/bin/bash

# Configuration
CONTAINER_NAME="skedulr-redis-test"
REDIS_PORT=6379

echo "🐳 Starting Skedulr Example Environment..."

# 1. Cleanup old container if exists
if [ "$(docker ps -aq -f name=${CONTAINER_NAME})" ]; then
    echo "Stopping existing container..."
    docker rm -f ${CONTAINER_NAME} > /dev/null 2>&1
fi

# 2. Start Redis
echo "Starting Redis on port ${REDIS_PORT}..."
docker run -d \
    --name ${CONTAINER_NAME} \
    -p ${REDIS_PORT}:6379 \
    redis:7-alpine > /dev/null

if [ $? -ne 0 ]; then
    echo "❌ Failed to start Docker container. Is Docker running?"
    exit 1
fi

# 3. Wait for Redis to be ready
echo "Waiting for Redis to wake up..."
max_retries=15
count=0
until docker exec ${CONTAINER_NAME} redis-cli ping > /dev/null 2>&1 || [ $count -eq $max_retries ]; do
    sleep 1
    ((count++))
done

if [ $count -eq $max_retries ]; then
    echo "❌ Redis failed to start in time."
    docker stop ${CONTAINER_NAME} > /dev/null
    exit 1
fi

echo "✅ Redis is ready!"

echo "🚀 Running Skedulr Example..."
echo "------------------------------------------------"
export REDIS_ADDR="localhost:${REDIS_PORT}"

trap "echo -e '\nStopping environment...'; docker rm -f ${CONTAINER_NAME} > /dev/null; exit 0" INT TERM

go run example/example.go

docker rm -f ${CONTAINER_NAME} > /dev/null
echo "👋 Done."
