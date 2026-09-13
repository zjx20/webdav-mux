package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// authCacheTTL 是一次成功的密码校验被缓存的时长。WebDAV 客户端每个请求都携带 Basic 认证，
// 浏览一个目录就可能发出几十个请求，不缓存的话每个请求都要付出一次 bcrypt 的开销。
// 缓存属于 state，配置重载（包括修改密码）后自动失效。
const authCacheTTL = 10 * time.Minute

// authCache 记住某个用户最近一次校验成功的密码摘要。摘要用每次加载随机生成的密钥做 HMAC，
// 内存里不保存明文密码，也不保存可以离线暴力破解的无盐哈希。
type authCache struct {
	mu      sync.Mutex
	digest  [sha256.Size]byte
	expires time.Time
}

func (c *authCache) hit(digest [sha256.Size]byte, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Before(c.expires) && hmac.Equal(c.digest[:], digest[:])
}

func (c *authCache) store(digest [sha256.Size]byte, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.digest = digest
	c.expires = now.Add(authCacheTTL)
}

func (st *state) passwordDigest(password string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, st.authKey)
	mac.Write([]byte(password))
	var d [sha256.Size]byte
	mac.Sum(d[:0])
	return d
}

// authenticate 校验请求的 Basic 认证，成功时返回对应用户。
func (s *Server) authenticate(r *http.Request, st *state) *user {
	name, password, ok := r.BasicAuth()
	if !ok {
		return nil
	}
	u := st.users[name]
	digest := st.passwordDigest(password)
	if u != nil && u.cache.hit(digest, time.Now()) {
		return u
	}

	// 缓存未命中的校验要占用一个 bcrypt 名额。命中缓存的请求不经过这里，
	// 所以暴力破解只会让未命中缓存的登录排队，不会拖垮正常的转发流量。
	select {
	case s.bcryptSlots <- struct{}{}:
	case <-r.Context().Done():
		return nil
	}
	defer func() { <-s.bcryptSlots }()

	if u == nil {
		bcrypt.CompareHashAndPassword(st.dummyHash, []byte(password))
		return nil
	}
	// 排队期间，同一用户的并发请求可能已经校验成功。
	if u.cache.hit(digest, time.Now()) {
		return u
	}
	if bcrypt.CompareHashAndPassword(u.hash, []byte(password)) != nil {
		return nil
	}
	u.cache.store(digest, time.Now())
	return u
}
