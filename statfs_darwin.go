package main

import "syscall"

type statfsT = syscall.Statfs_t

func statfs(path string, st *statfsT) error { return syscall.Statfs(path, st) }
