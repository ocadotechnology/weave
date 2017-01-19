// package IPsec provides primitives for establishing IPsec in the fastdp mode.
package ipsec

// TODO:
// 3. spiID -> type for the key
// 4. get rid of SPI type
// 5. protocol msg cleanup
// 6. better tracking of SPIs and cleanup
// 7. rename functions and arguments
// 8. atomic inserts
// 9. test with non-default ports
// 10. test on larger cluster
// 11. vishvananda/netlink comments
// 12. router/fastdp.go cleanup
// 13. locks granularity
// 14. user-configurable life-times
// 15. tests for rekeying
// 16. check flow
// 17. block incoming traffic as well

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"

	"github.com/coreos/go-iptables/iptables"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/crypto/hkdf"

	"github.com/weaveworks/mesh"
)

type SPI uint32

const (
	protoVsn = 1

	keySize   = 36 // AES-GCM key 32 bytes + 4 bytes salt
	nonceSize = 32 // HKDF nonce size

	mark    = uint32(0x1) << 17
	markStr = "0x20000/0x20000"

	tableMangle  = "mangle"
	tableFilter  = "filter"
	chainIn      = "WEAVE-IPSEC-IN"
	chainInMark  = "WEAVE-IPSEC-IN-MARK"
	chainOut     = "WEAVE-IPSEC-OUT"
	chainOutMark = "WEAVE-IPSEC-OUT-MARK"
)

// IPSec

type spiID [16]byte
type spiInfo struct {
	spi       SPI
	isDirOut  bool
	initRekey func() error
}

type IPSec struct {
	sync.RWMutex
	ipt *iptables.IPTables
	rc  *connRefCount

	spiInfo map[spiID]spiInfo
	spis    map[SPI]*spiInfo
}

func New() (*IPSec, error) {
	ipt, err := iptables.New()
	if err != nil {
		return nil, errors.Wrap(err, "iptables new")
	}

	ipsec := &IPSec{
		ipt:     ipt,
		rc:      newConnRefCount(),
		spiInfo: make(map[spiID]spiInfo),
		spis:    make(map[SPI]*spiInfo),
	}

	return ipsec, nil
}

func (ipsec *IPSec) Monitor() error {
	ch := make(chan netlink.XfrmMsg)
	errorCh := make(chan error)

	if err := netlink.XfrmMonitor(ch, nil, errorCh, nl.XFRM_MSG_EXPIRE); err != nil {
		return errors.Wrap(err, "xfrm monitor")
	}

	for {
		select {
		case err := <-errorCh:
			return err
		case msg := <-ch:
			if exp, ok := msg.(*netlink.XfrmMsgExpire); ok {

				spi := SPI(exp.XfrmState.Spi)

				if exp.Hard {

					ipsec.Lock()
					if spiInfo, ok := ipsec.spis[spi]; ok && !spiInfo.isDirOut {
						if err := ipsec.removeObsoleteProtectingRule(exp.XfrmState.Dst, exp.XfrmState.Src, spi); err != nil {
							ipsec.Unlock()
							return errors.Wrap(err, "removeObsoleteProtectingRule")

						}
					}
					delete(ipsec.spis, spi)
					ipsec.Unlock()

				} else {
					ipsec.Lock()

					if spiInfo, ok := ipsec.spis[spi]; ok && spiInfo.initRekey != nil {
						if err := spiInfo.initRekey(); err != nil {
							ipsec.Unlock()
							return errors.Wrap(err, "monitor init-rekey")
						}
					}

					ipsec.Unlock()
				}
			}
		}
	}
}

// SAremote->local
func (ipsec *IPSec) ProtectInit(localPeer, remotePeer mesh.PeerName, localIP, remoteIP net.IP, dstPort int, sessionKey *[32]byte, isRekey bool, send func([]byte) error) error {
	ipsec.Lock()
	defer ipsec.Unlock()

	if !isRekey && ipsec.rc.get(localPeer, remotePeer) > 1 {
		// IPSec has been already set up between the given peers
		return nil
	}

	spiKey := connRefKey(remotePeer, localPeer)
	if isRekey {
		if _, ok := ipsec.spiInfo[spiKey]; !ok {
			return fmt.Errorf("cannot find SPI by %x", spiKey)
		}
	}

	nonce, err := genNonce()
	if err != nil {
		return errors.Wrap(err, "generate nonce")
	}
	key, err := deriveKey(sessionKey[:], nonce, localPeer)
	if err != nil {
		return errors.Wrap(err, "derive key")
	}

	sa, err := netlink.XfrmStateAllocSpi(xfrmAllocSpiState(remoteIP, localIP))
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("ip xfrm state allocspi (in, %s, %s)", remoteIP, localIP))
	}

	spi := SPI(sa.Spi)
	if sa, err := xfrmState(remoteIP, localIP, spi, false, key); err == nil {
		if err := netlink.XfrmStateUpdate(sa); err != nil {
			return errors.Wrap(err, fmt.Sprintf("xfrm state update (in, %s, %s, 0x%x)", sa.Src, sa.Dst, sa.Spi))
		}
	} else {
		return errors.Wrap(err, "new xfrm state (in)")
	}

	if !isRekey {
		if err := ipsec.installProtectingRules(localIP, remoteIP, dstPort, spi); err != nil {
			return errors.Wrap(err, fmt.Sprintf("install protecting rules (%s, %s, %d, 0x%x)", localIP, remoteIP, dstPort, spi))
		}
	} else {
		if err := ipsec.installProtectingRuleAfterRekeying(localIP, remoteIP, spi); err != nil {
			return errors.Wrap(err, fmt.Sprintf("update protecting rules (%s, %s, %d, 0x%x)", localIP, remoteIP, spi))
		}
	}

	if err := send(composeCreateSA(nonce, spi)); err != nil {
		return errors.Wrap(err, "send CREATE_SA")
	}

	si := spiInfo{spi: spi, isDirOut: false}
	ipsec.spiInfo[spiKey] = si
	ipsec.spis[spi] = &si

	return nil
}

// SAlocal->remote
func (ipsec *IPSec) ProtectFinish(createSAMsg []byte, localPeer, remotePeer mesh.PeerName, localIP, remoteIP net.IP, dstPort int, sessionKey *[32]byte, rekey func() error) error {
	ipsec.Lock()
	defer ipsec.Unlock()

	if size := len(createSAMsg); size != createSASize {
		return fmt.Errorf("invalid CREATE_SA msg size: %d", size)
	}
	vsn, nonce, spi := parseCreateSA(createSAMsg)
	if vsn != protoVsn {
		return fmt.Errorf("unsupported vsn: %d", vsn)
	}

	spiKey := connRefKey(localPeer, remotePeer)
	_, isRekey := ipsec.spiInfo[spiKey]

	key, err := deriveKey(sessionKey[:], nonce, remotePeer)
	if err != nil {
		return errors.Wrap(err, "derive key")
	}

	if sa, err := xfrmState(localIP, remoteIP, spi, true, key); err == nil {
		if err := netlink.XfrmStateAdd(sa); err != nil {
			return errors.Wrap(err, fmt.Sprintf("xfrm state update (out, %s, %s, 0x%x)", sa.Src, sa.Dst, sa.Spi))
		}
	} else {
		return errors.Wrap(err, "new xfrm state (out)")
	}

	sp := xfrmPolicy(localIP, remoteIP, spi)
	if isRekey {
		if err := netlink.XfrmPolicyUpdate(sp); err != nil {
			return errors.Wrap(err, fmt.Sprintf("xfrm policy update (%s, %s, 0x%x)", localIP, remoteIP, spi))
		}
	} else {
		if err := netlink.XfrmPolicyAdd(sp); err != nil {
			return errors.Wrap(err, fmt.Sprintf("xfrm policy add (%s, %s, 0x%x)", localIP, remoteIP, spi))
		}
	}

	si := spiInfo{spi: spi, isDirOut: true, initRekey: rekey}
	ipsec.spiInfo[spiKey] = si
	ipsec.spis[spi] = &si

	return nil
}

func (ipsec *IPSec) Destroy(localPeer, remotePeer mesh.PeerName, localIP, remoteIP net.IP, remotePort int) error {
	ipsec.Lock()
	defer ipsec.Unlock()

	count := ipsec.rc.put(localPeer, remotePeer)
	switch {
	case count > 0:
		return nil
	case count < 0:
		return fmt.Errorf("IPSec invalid state")
	}

	inSPIKey := connRefKey(remotePeer, localPeer)
	inSPIInfo, ok := ipsec.spiInfo[inSPIKey]
	inSPI := inSPIInfo.spi
	if ok {
		inSA := &netlink.XfrmState{
			Src:   remoteIP,
			Dst:   localIP,
			Proto: netlink.XFRM_PROTO_ESP,
			Spi:   int(inSPI),
		}
		if err := netlink.XfrmStateDel(inSA); err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("xfrm state del (in, %s, %s, 0x%x)", inSA.Src, inSA.Dst, inSA.Spi))
		}
		delete(ipsec.spiInfo, inSPIKey)
		delete(ipsec.spis, inSPI)
	}

	outSPIKey := connRefKey(localPeer, remotePeer)
	if outSPIInfo, ok := ipsec.spiInfo[outSPIKey]; ok {
		if err := netlink.XfrmPolicyDel(xfrmPolicy(localIP, remoteIP, outSPIInfo.spi)); err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("xfrm policy del (%s, %s, 0x%x)", localIP, remoteIP, outSPIInfo.spi))
		}

		outSA := &netlink.XfrmState{
			Src:   localIP,
			Dst:   remoteIP,
			Proto: netlink.XFRM_PROTO_ESP,
			Spi:   int(outSPIInfo.spi),
		}
		if err := netlink.XfrmStateDel(outSA); err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("xfrm state del (out, %s, %s, 0x%x)", outSA.Src, outSA.Dst, outSA.Spi))
		}

		// TODO(mp) if not found inSPI???
		if err := ipsec.removeProtectingRules(localIP, remoteIP, remotePort, inSPI); err != nil {
			return errors.Wrap(err,
				fmt.Sprintf("remove protecting rules (%s, %s, %d, 0x%x)", localIP, remoteIP, remotePort, inSPI))
		}

		delete(ipsec.spiInfo, outSPIKey)
		delete(ipsec.spis, outSPIInfo.spi)
	}

	return nil

}

// Flush removes all policies/SAs established by us. Also, it removes chains and
// rules of iptables used for the marking. If destroy is true, the chains and
// the marking rule won't be re-created.
// TODO(mp) maybe use the security context (XFRM_SEC_CTX) to identify SAs/SPs created by us.
func (ipsec *IPSec) Flush(destroy bool) error {
	ipsec.Lock()
	defer ipsec.Unlock()

	policies, err := netlink.XfrmPolicyList(syscall.AF_INET)
	if err != nil {
		return errors.Wrap(err, "xfrm policy list")
	}
	for _, p := range policies {
		if p.Mark != nil && p.Mark.Value == mark && len(p.Tmpls) != 0 {
			spi := SPI(p.Tmpls[0].Spi)
			if err := netlink.XfrmPolicyDel(&p); err != nil {
				return errors.Wrap(err, fmt.Sprintf("xfrm policy del (%s, %s, 0x%x)", p.Src, p.Dst, spi))
			}
		}
	}

	states, err := netlink.XfrmStateList(syscall.AF_INET)
	if err != nil {
		return errors.Wrap(err, "xfrm state list")
	}
	for _, s := range states {
		if _, ok := ipsec.spis[SPI(s.Spi)]; ok {
			if err := netlink.XfrmStateDel(&s); err != nil {
				return errors.Wrap(err, fmt.Sprintf("xfrm state list (%s, %s, 0x%x)", s.Src, s.Dst, s.Spi))
			}
		}
	}

	if err := ipsec.resetIPTables(destroy); err != nil {
		return errors.Wrap(err, "reset ip tables")
	}

	return nil
}

// connRefCount

// Reference counting for IPsec establishments.
//
// Mesh might simultaneously create two connections for the same peer pair which
// could result in establishing IPsec multiple times.
type connRefCount struct {
	ref map[spiID]int
}

func newConnRefCount() *connRefCount {
	return &connRefCount{ref: make(map[spiID]int)}
}

func (rc *connRefCount) get(srcPeer, dstPeer mesh.PeerName) int {
	key := connRefKey(srcPeer, dstPeer)
	rc.ref[key]++

	return rc.ref[key]
}

func (rc *connRefCount) put(srcPeer, dstPeer mesh.PeerName) int {
	key := connRefKey(srcPeer, dstPeer)
	rc.ref[key]--

	return rc.ref[key]
}

func connRefKey(srcPeer, dstPeer mesh.PeerName) (key spiID) {
	binary.BigEndian.PutUint64(key[:], uint64(srcPeer))
	binary.BigEndian.PutUint64(key[8:], uint64(dstPeer))
	return
}

// iptables

// INBOUND:
// --------
//
// mangle:
// -A INPUT -j WEAVE-IPSEC-IN															# default
// -A WEAVE-IPSEC-IN -s $remote -d $local -m esp --espspi $spi -j WEAVE-IPSEC-IN-MARK	# ProtectInit
// -A WEAVE-IPSEC-IN-MARK --set-xmark $mark	-j MARK 									# default
//
// filter:
// -A INPUT -j WEAVE-IPSEC-IN																	# default
// -A WEAVE-IPSEC-IN -s $remote -d $local -p udp --dport $port -m mark ! --mark $mark -j DROP	# ProtectInit
//
//
// OUTBOUND:
// ---------
//
// mangle:
// -A OUTPUT -j WEAVE-IPSEC-OUT																	# default
// -A WEAVE-IPSEC-OUT -s $local -d $remote -p udp --dport $port -j WEAVE-IPSEC-OUT-MARK			# ProtectInit
// -A WEAVE-IPSEC-OUT-MARK -j MARK --set-xmark $mark											# default
//
// filter:
// -A OUTPUT ! -p esp -m policy --dir out --pol none -m mark --mark $mark -j DROP				# default

type chain struct {
	table string
	chain string
}
type rule struct {
	table    string
	chain    string
	rulespec []string
}

func (ipsec *IPSec) clearChains(chains []chain) error {
	for _, c := range chains {
		if err := ipsec.ipt.ClearChain(c.table, c.chain); err != nil {
			return errors.Wrap(err, fmt.Sprintf("iptables clear chain (%s, %s)", c.table, c.chain))
		}
	}
	return nil
}

func (ipsec *IPSec) deleteChains(chains []chain) error {
	for _, c := range chains {
		if err := ipsec.ipt.DeleteChain(c.table, c.chain); err != nil {
			return errors.Wrap(err, fmt.Sprintf("iptables delete chain (%s, %s)", c.table, c.chain))
		}
	}
	return nil
}

func (ipsec *IPSec) resetRules(rules []rule, destroy bool) error {
	for _, r := range rules {
		ok, err := ipsec.ipt.Exists(r.table, r.chain, r.rulespec...)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("iptables exists rule (%s, %s, %s)", r.table, r.chain, r.rulespec))
		}
		switch {
		case !destroy && !ok:
			if err := ipsec.ipt.Append(r.table, r.chain, r.rulespec...); err != nil {
				return errors.Wrap(err, fmt.Sprintf("iptables append rule (%s, %s, %s)", r.table, r.chain, r.rulespec))
			}
		case destroy && ok:
			if err := ipsec.ipt.Delete(r.table, r.chain, r.rulespec...); err != nil {
				return errors.Wrap(err, fmt.Sprintf("iptables delete rule (%s, %s, %s)", r.table, r.chain, r.rulespec))
			}
		}
	}
	return nil
}

func (ipsec *IPSec) resetIPTables(destroy bool) error {
	chains := []chain{
		{tableMangle, chainIn},
		{tableMangle, chainInMark},
		{tableFilter, chainIn},
		{tableMangle, chainOut},
		{tableMangle, chainOutMark},
	}
	rules := []rule{
		{tableMangle, "INPUT", []string{"-j", chainIn}},
		{tableMangle, chainInMark, []string{"-j", "MARK", "--set-xmark", markStr}},
		{tableFilter, "INPUT", []string{"-j", chainIn}},
		{tableMangle, "OUTPUT", []string{"-j", chainOut}},
		{tableMangle, chainOutMark, []string{"-j", "MARK", "--set-xmark", markStr}},
		{tableFilter, "OUTPUT",
			[]string{
				"!", "-p", "esp",
				"-m", "policy", "--dir", "out", "--pol", "none",
				"-m", "mark", "--mark", markStr,
				"-j", "DROP"}},
	}

	if err := ipsec.clearChains(chains); err != nil {
		return err
	}

	if err := ipsec.resetRules(rules, destroy); err != nil {
		return err
	}

	if destroy {
		if err := ipsec.deleteChains(chains); err != nil {
			return err
		}
	}

	return nil
}

func protectingInRule(srcIP, dstIP net.IP, inSPI SPI) rule {
	return rule{tableMangle, chainIn,
		[]string{
			"-s", dstIP.String(), "-d", srcIP.String(),
			"-p", "esp",
			"-m", "esp", "--espspi", "0x" + strconv.FormatUint(uint64(inSPI), 16),
			"-j", chainInMark,
		}}
}

func protectingRules(srcIP, dstIP net.IP, dstPort int, inSPI SPI) []rule {
	return []rule{
		protectingInRule(srcIP, dstIP, inSPI),
		{tableFilter, chainIn,
			[]string{
				"-s", dstIP.String(), "-d", srcIP.String(),
				"-p", "udp", "--dport", strconv.FormatUint(uint64(dstPort), 10),
				"-m", "mark", "!", "--mark", markStr,
				"-j", "DROP",
			}},
		{tableMangle, chainOut,
			[]string{
				"-s", srcIP.String(), "-d", dstIP.String(),
				"-p", "udp", "--dport", strconv.FormatUint(uint64(dstPort), 10),
				"-j", chainOutMark,
			}},
	}
}

func (ipsec *IPSec) installProtectingRules(srcIP, dstIP net.IP, dstPort int, inSPI SPI) error {
	rules := protectingRules(srcIP, dstIP, dstPort, inSPI)
	for _, r := range rules {
		if err := ipsec.ipt.AppendUnique(r.table, r.chain, r.rulespec...); err != nil {
			return errors.Wrap(err, fmt.Sprintf("iptables append unique (%s, %s, %s)", r.table, r.chain, r.rulespec))
		}
	}
	return nil
}

func (ipsec *IPSec) removeProtectingRules(srcIP, dstIP net.IP, dstPort int, inSPI SPI) error {
	if err := ipsec.resetRules(protectingRules(srcIP, dstIP, dstPort, inSPI), true); err != nil {
		return err
	}
	return nil
}

// TODO(mp) swap src/dst
func (ipsec *IPSec) installProtectingRuleAfterRekeying(srcIP, dstIP net.IP, inSPI SPI) error {
	r := protectingInRule(srcIP, dstIP, inSPI)
	if err := ipsec.ipt.AppendUnique(r.table, r.chain, r.rulespec...); err != nil {
		return errors.Wrap(err, fmt.Sprintf("iptables append unique (%s, %s, %s)", r.table, r.chain, r.rulespec))
	}
	return nil
}

func (ipsec *IPSec) removeObsoleteProtectingRule(srcIP, dstIP net.IP, inSPI SPI) error {
	r := protectingInRule(srcIP, dstIP, inSPI)
	if err := ipsec.ipt.Delete(r.table, r.chain, r.rulespec...); err != nil {
		return errors.Wrap(err, fmt.Sprintf("iptables delete unique (%s, %s, %s)", r.table, r.chain, r.rulespec))
	}
	return nil
}

// xfrm

func xfrmAllocSpiState(srcIP, dstIP net.IP) *netlink.XfrmState {
	return &netlink.XfrmState{
		Src:          srcIP,
		Dst:          dstIP,
		Proto:        netlink.XFRM_PROTO_ESP,
		Mode:         netlink.XFRM_MODE_TRANSPORT,
		ReplayWindow: 32,
	}
}

func xfrmState(srcIP, dstIP net.IP, spi SPI, isOut bool, key []byte) (*netlink.XfrmState, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("key should be %d bytes long", keySize)
	}

	state := xfrmAllocSpiState(srcIP, dstIP)

	state.Spi = int(spi)
	state.Aead = &netlink.XfrmStateAlgo{
		Name:   "rfc4106(gcm(aes))",
		Key:    key,
		ICVLen: 128,
	}

	state.Limits = netlink.XfrmStateLimits{
		PacketHard: 100,
		TimeHard:   14,
	}
	if isOut {
		state.Limits.PacketSoft = 50
		state.Limits.TimeSoft = 10
	}

	return state, nil
}

func xfrmPolicy(srcIP, dstIP net.IP, spi SPI) *netlink.XfrmPolicy {
	ipMask := []byte{0xff, 0xff, 0xff, 0xff} // /32

	return &netlink.XfrmPolicy{
		Src:   &net.IPNet{IP: srcIP, Mask: ipMask},
		Dst:   &net.IPNet{IP: dstIP, Mask: ipMask},
		Proto: syscall.IPPROTO_UDP,
		Dir:   netlink.XFRM_DIR_OUT,
		Mark: &netlink.XfrmMark{
			Value: mark,
			Mask:  mark,
		},
		Tmpls: []netlink.XfrmPolicyTmpl{
			{
				Src:   srcIP,
				Dst:   dstIP,
				Proto: netlink.XFRM_PROTO_ESP,
				Mode:  netlink.XFRM_MODE_TRANSPORT,
				Spi:   int(spi),
			},
		},
		// TODO(mp) limits
	}
}

// Key derivation

func genNonce() ([]byte, error) {
	buf := make([]byte, nonceSize)
	n, err := rand.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("crypto rand failed: %s", err)
	}
	if n != nonceSize {
		return nil, fmt.Errorf("not enough of random data: %d", n)
	}
	return buf, nil
}

func deriveKey(sessionKey []byte, nonce []byte, peerName mesh.PeerName) ([]byte, error) {
	key := make([]byte, keySize)

	info := make([]byte, 8)
	binary.BigEndian.PutUint64(info, uint64(peerName))

	hkdf := hkdf.New(sha256.New, sessionKey, nonce, info)

	n, err := io.ReadFull(hkdf, key)
	if err != nil {
		return nil, err
	}
	if n != keySize {
		return nil, fmt.Errorf("derived too short key: %d", n)
	}

	return key, nil
}

// Protocol Messages

const createSASize = 1 + nonceSize + 32

// | 1: VSN | 32: Nonce | 32: SPI |
func composeCreateSA(nonce []byte, spi SPI) []byte {
	msg := make([]byte, createSASize)

	msg[0] = protoVsn
	copy(msg[1:(1+nonceSize)], nonce)
	binary.BigEndian.PutUint32(msg[1+nonceSize:], uint32(spi))

	return msg
}

func parseCreateSA(msg []byte) (uint8, []byte, SPI) {
	nonce := make([]byte, nonceSize)
	copy(nonce, msg[1:(1+nonceSize)])
	spi := SPI(binary.BigEndian.Uint32(msg[1+nonceSize:]))

	return msg[0], nonce, spi
}