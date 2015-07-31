// @description wechat 是腾讯微信公众平台 api 的 golang 语言封装
// @link        https://github.com/chanxuehong/wechat for the canonical source repository
// @license     https://github.com/chanxuehong/wechat/blob/master/LICENSE
// @authors     chanxuehong(chanxuehong@gmail.com)

// +build !wechatdebug

package component

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/chanxuehong/wechat/mp"
	"github.com/chanxuehong/wechat/util"
)

// 微信服务器请求 http body
type RequestHttpBody struct {
	XMLName struct{} `xml:"xml" json:"-"`

	AppId        string `xml:"AppId"`
	EncryptedMsg string `xml:"Encrypt"`
}

// ServeHTTP 处理 http 消息请求
//  NOTE: 调用者保证所有参数有效
func ServeHTTP(w http.ResponseWriter, r *http.Request, queryValues url.Values, srv Server, errHandler mp.ErrorHandler) {
	switch r.Method {
	case "POST": // 消息处理
		switch encryptType := queryValues.Get("encrypt_type"); encryptType {
		case "aes":
			msgSignature1 := queryValues.Get("msg_signature")
			if msgSignature1 == "" {
				errHandler.ServeError(w, r, errors.New("msg_signature is empty"))
				return
			}
			if len(msgSignature1) != 40 { // sha1
				err := fmt.Errorf("the length of msg_signature mismatch, have: %d, want: 40", len(msgSignature1))
				errHandler.ServeError(w, r, err)
				return
			}

			timestampStr := queryValues.Get("timestamp")
			if timestampStr == "" {
				errHandler.ServeError(w, r, errors.New("timestamp is empty"))
				return
			}

			timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
			if err != nil {
				err = errors.New("can not parse timestamp to int64: " + timestampStr)
				errHandler.ServeError(w, r, err)
				return
			}

			nonce := queryValues.Get("nonce")
			if nonce == "" {
				errHandler.ServeError(w, r, errors.New("nonce is empty"))
				return
			}

			var requestHttpBody RequestHttpBody
			if err := xml.NewDecoder(r.Body).Decode(&requestHttpBody); err != nil {
				errHandler.ServeError(w, r, err)
				return
			}

			haveAppId := requestHttpBody.AppId
			wantAppId := srv.AppId()
			if wantAppId != "" {
				if len(haveAppId) != len(wantAppId) {
					err = fmt.Errorf("the RequestHttpBody's AppId mismatch, have: %s, want: %s", haveAppId, wantAppId)
					errHandler.ServeError(w, r, err)
					return
				}
				if subtle.ConstantTimeCompare([]byte(haveAppId), []byte(wantAppId)) != 1 {
					err = fmt.Errorf("the RequestHttpBody's AppId mismatch, have: %s, want: %s", haveAppId, wantAppId)
					errHandler.ServeError(w, r, err)
					return
				}
			}

			token := srv.Token()

			// 验证签名
			msgSignature2 := util.MsgSign(token, timestampStr, nonce, requestHttpBody.EncryptedMsg)
			if subtle.ConstantTimeCompare([]byte(msgSignature1), []byte(msgSignature2)) != 1 {
				err = fmt.Errorf("check msg_signature failed, input: %s, local: %s", msgSignature1, msgSignature2)
				errHandler.ServeError(w, r, err)
				return
			}

			// 解密
			encryptedMsgBytes, err := base64.StdEncoding.DecodeString(requestHttpBody.EncryptedMsg)
			if err != nil {
				errHandler.ServeError(w, r, err)
				return
			}

			aesKey := srv.CurrentAESKey()
			random, rawMsgXML, aesAppId, err := util.AESDecryptMsg(encryptedMsgBytes, aesKey)
			if err != nil {
				// 尝试用上一次的 AESKey 来解密
				lastAESKey, isLastAESKeyValid := srv.LastAESKey()
				if !isLastAESKeyValid {
					errHandler.ServeError(w, r, err)
					return
				}

				aesKey = lastAESKey // NOTE

				random, rawMsgXML, aesAppId, err = util.AESDecryptMsg(encryptedMsgBytes, aesKey)
				if err != nil {
					errHandler.ServeError(w, r, err)
					return
				}
			}
			if haveAppId != string(aesAppId) {
				err = fmt.Errorf("the RequestHttpBody's ToUserName(==%s) mismatch the AppId with aes encrypt(==%s)", haveAppId, aesAppId)
				errHandler.ServeError(w, r, err)
				return
			}

			// 解密成功, 解析 MixedMessage
			var mixedMsg MixedMessage
			if err := xml.Unmarshal(rawMsgXML, &mixedMsg); err != nil {
				errHandler.ServeError(w, r, err)
				return
			}

			// 安全考虑再次验证 AppId
			if haveAppId != mixedMsg.AppId {
				err = fmt.Errorf("the RequestHttpBody's AppId(==%s) mismatch the MixedMessage's AppId(==%s)", haveAppId, mixedMsg.AppId)
				errHandler.ServeError(w, r, err)
				return
			}

			// 成功, 交给 MessageHandler
			req := &Request{
				Token: token,

				HttpRequest: r,
				QueryValues: queryValues,

				MsgSignature: msgSignature1,
				EncryptType:  encryptType,
				Timestamp:    timestamp,
				Nonce:        nonce,

				RawMsgXML: rawMsgXML,
				MixedMsg:  &mixedMsg,

				AESKey: aesKey,
				Random: random,
				AppId:  haveAppId,
			}
			srv.MessageHandler().ServeMessage(w, req)

		default: // 未知的加密类型
			err := errors.New("unknown encrypt_type: " + encryptType)
			errHandler.ServeError(w, r, err)
			return
		}

	case "GET": // 首次验证
		signature1 := queryValues.Get("signature")
		if signature1 == "" {
			errHandler.ServeError(w, r, errors.New("signature is empty"))
			return
		}
		if len(signature1) != 40 { // sha1
			err := fmt.Errorf("the length of signature mismatch, have: %d, want: 40", len(signature1))
			errHandler.ServeError(w, r, err)
			return
		}

		timestamp := queryValues.Get("timestamp")
		if timestamp == "" {
			errHandler.ServeError(w, r, errors.New("timestamp is empty"))
			return
		}

		nonce := queryValues.Get("nonce")
		if nonce == "" {
			errHandler.ServeError(w, r, errors.New("nonce is empty"))
			return
		}

		echostr := queryValues.Get("echostr")
		if echostr == "" {
			errHandler.ServeError(w, r, errors.New("echostr is empty"))
			return
		}

		signature2 := util.Sign(srv.Token(), timestamp, nonce)
		if subtle.ConstantTimeCompare([]byte(signature1), []byte(signature2)) != 1 {
			err := fmt.Errorf("check signature failed, input: %s, local: %s", signature1, signature2)
			errHandler.ServeError(w, r, err)
			return
		}

		io.WriteString(w, echostr)
	}
}
