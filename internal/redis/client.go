package redis

import (
	redisClient "github.com/redis/go-redis/v9"
)

func NewClient(addr string) *redisClient.Client {
	opts := redisClient.Options{
		Addr: addr,
	}

	client := redisClient.NewClient(&opts)

	return client

}
