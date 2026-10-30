# 端到端测试用的目标服务器：systemd + sshd + sudo（Oracle Linux 9）。
# 运行：docker run -d --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw -p 127.0.0.1:2222:22 ferry-e2e:ol9
FROM oraclelinux:9
RUN dnf install -y systemd openssh-server procps-ng iproute iptables-nft shadow-utils util-linux sudo xz && dnf clean all \
 && systemctl enable sshd \
 && useradd -r -m -s /sbin/nologin demo \
 && useradd -m deployer \
 && echo 'deployer ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/deployer && chmod 440 /etc/sudoers.d/deployer \
 && mkdir -p /root/.ssh /home/deployer/.ssh && chmod 700 /root/.ssh /home/deployer/.ssh \
 && chown deployer: /home/deployer/.ssh \
 && echo 'deployer:ferry-e2e-pw' | chpasswd \
 && printf 'PasswordAuthentication yes\nKbdInteractiveAuthentication yes\n' > /etc/ssh/sshd_config.d/10-e2e.conf
# 测试用的「java」：-version 报 /etc/fakejava-version 里的版本（默认 17），其他情况启动测试服务
COPY fakejava.sh /usr/local/bin/fakejava
RUN chmod 755 /usr/local/bin/fakejava
# Node 服务端应用（type: node）用；Python 系统自带（type: python）
RUN dnf install -y nodejs npm && dnf clean all
STOPSIGNAL SIGRTMIN+3
CMD ["/usr/sbin/init"]
