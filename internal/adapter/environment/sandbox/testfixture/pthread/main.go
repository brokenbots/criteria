//go:build linux

// pthread is a test helper that runs inside the sandbox shim, applies the
// shim restrictions via ApplyEnv, and then attempts to spawn N POSIX threads
// using pthread_create. It prints THREADS_OK count=<n> with the number of
// threads successfully created. This exercises RLIMIT_NPROC plumbing without
// relying on the full GitHub Copilot CLI binary.
package main

/*
#include <pthread.h>
#include <semaphore.h>
#include <stdlib.h>

// Threads wait on this semaphore so they stay alive while subsequent
// pthread_create calls are made. This makes the surrogate sensitive to
// RLIMIT_NPROC because live threads count against the per-user limit.
static sem_t g_sem;

static void *wait_thread(void *arg) {
	sem_wait(&g_sem);
	return NULL;
}

int spawn_pthreads(int n) {
	sem_init(&g_sem, 0, 0);
	pthread_t *threads = malloc(sizeof(pthread_t) * n);
	if (threads == NULL) {
		return 0;
	}
	int ok = 0;
	for (int i = 0; i < n; i++) {
		if (pthread_create(&threads[i], NULL, wait_thread, NULL) != 0) {
			break;
		}
		ok++;
	}
	// Release every thread that was successfully created.
	for (int i = 0; i < ok; i++) {
		sem_post(&g_sem);
	}
	for (int i = 0; i < ok; i++) {
		pthread_join(threads[i], NULL);
	}
	free(threads);
	return ok;
}
*/
import "C"

import (
	"fmt"
	"os"
	"strconv"

	"github.com/brokenbots/criteria/internal/adapter/environment/sandbox"
)

func main() {
	err := sandbox.ApplyEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim error:", err)
		os.Exit(1)
	}
	n := 100
	if len(os.Args) > 1 {
		v, err := strconv.Atoi(os.Args[1])
		if err == nil && v > 0 {
			n = v
		}
	}
	count := int(C.spawn_pthreads(C.int(n)))
	fmt.Printf("THREADS_OK count=%d\n", count)
}
