// Browser side of the media connection. The server always makes the offers: two
// receive-only slots for our own microphone and camera, and one track per other participant.
// Our devices are attached to the slots with replaceTrack, so switching a device on or off
// needs no renegotiation.
class RoomMedia {
  constructor(send, onChange) {
    this.send = send;
    this.onChange = onChange;
    this.pc = null;
    this.slots = {};   // kind -> RTCRtpTransceiver for our own media
    this.local = {};   // kind -> MediaStreamTrack from our devices
    this.remote = {};  // participant id -> MediaStream
    this.queue = Promise.resolve();
    this.lastRestart = 0;
  }

  start(iceServers) {
    this.close();
    const pc = new RTCPeerConnection({ iceServers });
    this.pc = pc;
    pc.onicecandidate = (e) => { if (e.candidate) this.send('rtc.candidate', e.candidate.toJSON()); };
    pc.ontrack = (e) => {
      const stream = e.streams[0];
      if (!stream || !stream.id.startsWith('p-')) return;
      const pid = stream.id.slice(2);
      this.remote[pid] = stream;
      stream.onremovetrack = () => {
        if (!stream.getTracks().length) delete this.remote[pid];
        this.onChange();
      };
      this.onChange();
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === 'failed' && Date.now() - this.lastRestart > 10000) {
        this.lastRestart = Date.now();
        this.send('rtc.restart');
      }
    };
  }

  // Offers and candidates are applied strictly in order.
  enqueue(fn) {
    this.queue = this.queue.then(fn).catch((err) => console.error('media', err));
    return this.queue;
  }

  offer(desc) {
    return this.enqueue(async () => {
      const pc = this.pc;
      if (!pc) return;
      await pc.setRemoteDescription(desc);
      const upload = recvonlyMids(desc.sdp);
      for (const t of pc.getTransceivers()) {
        if (!upload.has(t.mid)) continue;
        const kind = t.receiver.track.kind;
        t.direction = 'sendonly';
        if (this.slots[kind] !== t) {
          this.slots[kind] = t;
          await t.sender.replaceTrack(this.local[kind] || null);
        }
      }
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      this.send('rtc.answer', { type: answer.type, sdp: pc.localDescription.sdp });
    });
  }

  candidate(c) {
    return this.enqueue(async () => { if (this.pc) await this.pc.addIceCandidate(c); });
  }

  // enable asks the browser for the device (it shows its own consent prompt) and starts sending.
  async enable(kind) {
    if (!this.local[kind]) {
      const stream = await navigator.mediaDevices.getUserMedia(kind === 'audio'
        ? { audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } }
        : { video: { width: { ideal: 640 }, height: { ideal: 360 }, frameRate: { ideal: 24 } } });
      this.local[kind] = stream.getTracks()[0];
    }
    await this.slots[kind]?.sender.replaceTrack(this.local[kind]);
    this.onChange();
  }

  // disable stops the device, so the camera light goes off too.
  disable(kind) {
    const track = this.local[kind];
    if (!track) return;
    track.stop();
    delete this.local[kind];
    this.slots[kind]?.sender.replaceTrack(null).catch(() => {});
    this.onChange();
  }

  disableAll() {
    this.disable('audio');
    this.disable('video');
  }

  close() {
    if (this.pc) this.pc.close();
    this.pc = null;
    this.slots = {};
    this.remote = {};
    this.queue = Promise.resolve();
  }
}

// recvonlyMids returns the mids of the server's receive-only sections: our upload slots.
function recvonlyMids(sdp) {
  const mids = new Set();
  for (const section of sdp.split(/\r?\nm=/).slice(1)) {
    const mid = section.match(/\r?\na=mid:(\S+)/);
    if (mid && /\r?\na=recvonly/.test(section)) mids.add(mid[1]);
  }
  return mids;
}
