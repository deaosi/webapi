package panel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
)

// adminKeySealer 用面板管理员密钥派生 AES-256-GCM 密钥，把调用密钥明文封成密文。
//
// 为什么派生而不是随机：管理员密钥本来就只存在服务器上，用它派生意味着
// 「更换管理员密钥」会同时作废所有旧密文——这是可接受且可解释的语义
// （Reveal 返回不可恢复、Rotate 用当前密钥重新封装），换来的是不需要再管一份
// 额外的存储密钥，也不会出现"密钥文件丢了就全废"的单点。
//
// key 是函数而不是字符串：面板支持热加载配置（cfg.Live），管理员密钥可能在
// 运行期更换，每次读写都必须用当前值，否则换完密钥连新写的密文都解不开。
type adminKeySealer struct {
	key func() string
}

// sealerDomain 派生时的域分隔前缀，避免与其他"用管理员密钥做派生"的用途撞车。
const sealerDomain = "wb2api/apikey-seal/v1"

var errNoAdminKey = errors.New("administrator key is not configured")

func (s adminKeySealer) derived() ([]byte, error) {
	material := s.key()
	if material == "" {
		return nil, errNoAdminKey
	}
	h := sha256.New()
	h.Write([]byte(sealerDomain))
	h.Write([]byte{0})
	h.Write([]byte(material))
	return h.Sum(nil), nil
}

// Seal 返回 base64url(nonce || ciphertext)。
func (s adminKeySealer) Seal(plaintext string) (string, error) {
	key, err := s.derived()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// Open 解开 Seal 的结果；任何异常（换过管理员密钥、密文损坏、格式不对）都返回 error，
// 调用方统一翻译成 apikeys.ErrSecretUnavailable。
func (s adminKeySealer) Open(blob string) (string, error) {
	key, err := s.derived()
	if err != nil {
		return "", err
	}
	raw, err := base64.RawURLEncoding.DecodeString(blob)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("sealed API key is truncated")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// 编译期确认它满足 store 的封存接口。
var _ apikeys.Sealer = adminKeySealer{}

// installAPIKeySealer 把面板的管理员密钥解析方式交给调用密钥库（构造时调用一次）。
func (p *Panel) installAPIKeySealer() {
	if p.cfg.APIKeys == nil {
		return
	}
	p.cfg.APIKeys.SetSealer(adminKeySealer{key: p.apiKey})
}
