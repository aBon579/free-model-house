package lane

import (
	"crypto/x509"
	"fmt"
	"os"
	"sync"
)

// CA 证书的显式配置。
//
// ⚠️ 容器是 `FROM scratch`，降权后进程是 nobody。
// 若 `/etc/ssl/certs/ca-certificates.crt` 对它不可读，Go 就没有证书池，
// 所有 HTTPS 都会失败（症状：模型清单全空、探测全失败）。
// 所以在降权**之前**把证书读进内存。
//
// 读不到不算错 —— Windows / 开发机上本来就没这个路径。

// caCertPath 是 CA 证书包的路径。
const caCertPath = "/etc/ssl/certs/ca-certificates.crt"

var (
	caOnce      sync.Once
	caPool      *x509.CertPool
	caLoadErr   error
	caAvailable bool
)

// LoadCACerts 读 CA 证书包并装进一个 CertPool。
//
// ⚠️ 必须在降权之前调用 —— 降权后就读不到了。
func LoadCACerts() (*x509.CertPool, bool, error) {
	return loadCACerts()
}

// loadCACerts 读 CA 证书包并装进一个 CertPool。
func loadCACerts() (*x509.CertPool, bool, error) {
	caOnce.Do(func() {
		data, err := os.ReadFile(caCertPath)
		if err != nil {
			caLoadErr = err
			return
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			caLoadErr = fmt.Errorf("%s 里没有可用的证书", caCertPath)
			return
		}
		caPool = pool
		caAvailable = true
	})
	return caPool, caAvailable, caLoadErr
}

// CAPool 返回加载好的 CA 证书池。
//
// ⚠️ 返回 nil 表示"用系统默认" —— 那在非容器环境里是正常的。
func CAPool() *x509.CertPool {
	pool, ok, _ := loadCACerts()
	if !ok {
		return nil
	}
	return pool
}

// CAStatus 报告 CA 证书的加载情况（给诊断用）。
func CAStatus() (path string, loaded bool, size int64, err error) {
	pool, ok, lerr := loadCACerts()
	_ = pool
	path = caCertPath
	loaded = ok
	err = lerr
	if st, serr := os.Stat(caCertPath); serr == nil {
		size = st.Size()
	}
	return
}

// CACertCount 返回加载进来的证书数量（给日志用）。
func CACertCount() int {
	pool, ok, _ := loadCACerts()
	if !ok || pool == nil {
		return 0
	}
	return len(pool.Subjects()) //nolint:staticcheck // 仅用于日志计数
}
