package terminal

// Names：同一project ID永远得到同一tmux socket与会话名，保证重连回到原会话。
func Names(projectID string) (socket, session string) {
	return projectID, "main"
}

// EnsureSessionCmds：附着前必须依次尝试的命令（第一条失败时才执行第二条）。
// 不使用new-session -A的前台形式：容器PID 1不是tmux，会话一律detached创建。
func EnsureSessionCmds(containerName, projectID string) [][]string {
	return [][]string{
		{"docker", "exec", "-e", "LANG=C.UTF-8", "-e", "LC_ALL=C.UTF-8", containerName, "tmux", "-L", projectID, "has-session", "-t", "main"},
		{"docker", "exec", "-e", "LANG=C.UTF-8", "-e", "LC_ALL=C.UTF-8", containerName, "tmux", "-L", projectID, "new-session", "-d", "-s", "main", "-c", "/workspace", "claude"},
	}
}

// AttachCmd必须带-t（审查§2.1）：容器内不分配TTY时tmux attach会直接失败；
// 宿主机侧的TTY由creack/pty提供给docker CLI进程，两者缺一不可。
func AttachCmd(containerName, projectID string) []string {
	return []string{"docker", "exec", "-it", "-e", "LANG=C.UTF-8", "-e", "LC_ALL=C.UTF-8", containerName,
		"tmux", "-L", projectID, "attach-session", "-t", "main"}
}
