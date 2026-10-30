package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/pkg/sftp"
)

// 连接断了、卡住了可以重新连接后接着传；没权限、磁盘满不行
func TestConnLost(t *testing.T) {
	for _, err := range []error{sftp.ErrSSHFxConnectionLost, io.EOF, fmt.Errorf("上传 x：%w", ErrStalled), errors.New("write tcp: connection reset by peer"), errors.New("broken pipe")} {
		if !ConnLost(err) {
			t.Errorf("%v 应算连接断了", err)
		}
	}
	for _, err := range []error{nil, os.ErrPermission, errors.New("no space left on device")} {
		if ConnLost(err) {
			t.Errorf("%v 不应算连接断了", err)
		}
	}
}

// 记下写完的片：同一片只算一次；远程文件没了时这个文件从头传
func TestUploadState(t *testing.T) {
	st := &UploadState{}
	st.init()
	st.mark(0, 0, chunkSize)
	st.mark(0, 0, chunkSize)
	st.mark(1, chunkSize, 100)
	if st.Done() != chunkSize+100 || !st.finished(0, 0) || st.finished(0, chunkSize) {
		t.Fatalf("done=%d", st.Done())
	}
	st.reset(0)
	if st.Done() != 100 || st.finished(0, 0) || !st.finished(1, chunkSize) {
		t.Fatalf("reset 后 done=%d", st.Done())
	}
}
