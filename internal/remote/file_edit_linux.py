import contextlib
import errno
import fcntl
import json
import os
import stat
import sys
import tempfile


def stamp(info):
    return ":".join(str(value) for value in (
        info.st_dev, info.st_ino, info.st_ctime_ns, info.st_mtime_ns,
        info.st_size, info.st_mode, info.st_uid, info.st_gid, info.st_nlink,
    ))


def attributes(fd):
    return {key: os.getxattr(fd, key) for key in os.listxattr(fd)}


def replace(requested, target, expected):
    content = sys.stdin.buffer.read(1048577)
    if len(content) > 1048576:
        return {"kind": "metadata", "message": "编辑内容超过 1 MiB"}
    parent, name = os.path.split(target)
    with contextlib.ExitStack() as stack:
        parent_fd = os.open(parent, os.O_DIRECTORY | os.O_PATH)
        stack.callback(os.close, parent_fd)
        if os.path.realpath(requested) != target:
            return {"kind": "changed"}
        original = os.open(name, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent_fd)
        stack.callback(os.close, original)
        info = os.fstat(original)
        if not stat.S_ISREG(info.st_mode) or stamp(info) != expected:
            return {"kind": "changed"}
        if info.st_nlink != 1:
            return {"kind": "metadata", "message": "文件有多个硬链接，无法安全替换"}
        try:
            fcntl.flock(original, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return {"kind": "changed"}
        saved_attributes = attributes(original)
        if "security.capability" in saved_attributes:
            return {"kind": "metadata", "message": "文件带有 Linux capabilities，无法安全编辑"}
        # The private directory keeps the staged inode inaccessible to other
        # users, even when the destination parent is shared.
        stage = tempfile.mkdtemp(prefix=".ferry-edit-", dir="/proc/self/fd/" + str(parent_fd))
        remove_file = False
        try:
            stage_fd = os.open(stage, os.O_DIRECTORY | os.O_NOFOLLOW | os.O_PATH)
            stack.callback(os.close, stage_fd)
            stage_info = os.fstat(stage_fd)
            if stage_info.st_uid != os.geteuid() or stage_info.st_mode & 0o077:
                return {"kind": "metadata", "message": "无法确认临时目录归当前操作用户独占"}
            fd = os.open("file", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=stage_fd)
            remove_file = True
            with os.fdopen(fd, "wb") as output:
                output.write(content)
                output.flush()
                fd = output.fileno()
                owner = os.fstat(fd)
                if (owner.st_uid, owner.st_gid) != (info.st_uid, info.st_gid):
                    os.fchown(fd, info.st_uid, info.st_gid)
                os.fchmod(fd, stat.S_IMODE(info.st_mode))
                current = attributes(fd)
                for key in current.keys() - saved_attributes.keys():
                    os.removexattr(fd, key)
                for key, value in saved_attributes.items():
                    if current.get(key) != value:
                        os.setxattr(fd, key, value)
                if attributes(fd) != saved_attributes or os.fstat(fd).st_mode != info.st_mode:
                    return {"kind": "metadata", "message": "无法完整保留文件权限及扩展属性"}
                os.fsync(fd)
                if os.path.realpath(requested) != target:
                    return {"kind": "changed"}
                if stamp(os.stat(requested)) != expected:
                    return {"kind": "changed"}
                if stamp(os.stat(name, dir_fd=parent_fd, follow_symlinks=False)) != expected:
                    return {"kind": "changed"}
                os.replace("file", name, src_dir_fd=stage_fd, dst_dir_fd=parent_fd)
                published = stamp(os.fstat(fd))
        finally:
            if remove_file:
                with contextlib.suppress(OSError):
                    os.unlink("file", dir_fd=stage_fd)
            with contextlib.suppress(OSError):
                os.rmdir(stage)
        return {"stamp": published}


try:
    if sys.argv[1] == "probe":
        info = os.stat(sys.argv[2], follow_symlinks=False)
        result = {"stamp": stamp(info), "links": info.st_nlink}
    else:
        result = replace(*sys.argv[2:])
except OSError as error:
    if error.errno in (errno.EACCES, errno.EPERM):
        result = {"kind": "permission", "message": error.strerror}
    elif error.errno in (errno.ENOENT, errno.ELOOP):
        result = {"kind": "changed"}
    else:
        result = {"kind": "metadata", "message": error.strerror}
print(json.dumps(result))
