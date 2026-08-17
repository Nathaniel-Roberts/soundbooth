// systap — minimal system-audio tap for soundbooth.
// Captures all system audio via ScreenCaptureKit (macOS 13+, Screen
// Recording permission only, no driver, no sudo) and writes raw
// interleaved Float32 stereo 48 kHz PCM to stdout until SIGINT/SIGTERM.
import AVFoundation
import CoreMedia
import Foundation
import ScreenCaptureKit

final class Tap: NSObject, SCStreamOutput, SCStreamDelegate {
    let out = FileHandle.standardOutput

    func stream(_ stream: SCStream, didOutputSampleBuffer sb: CMSampleBuffer, of type: SCStreamOutputType) {
        guard type == .audio, sb.isValid else { return }
        try? sb.withAudioBufferList { abl, _ in
            let buffers = Array(abl)
            if buffers.count == 1, let data = buffers[0].mData {
                // already interleaved
                out.write(Data(bytes: data, count: Int(buffers[0].mDataByteSize)))
            } else if buffers.count >= 2,
                      let l = buffers[0].mData, let r = buffers[1].mData {
                // non-interleaved stereo: interleave L/R
                let frames = Int(buffers[0].mDataByteSize) / MemoryLayout<Float32>.size
                let lp = l.assumingMemoryBound(to: Float32.self)
                let rp = r.assumingMemoryBound(to: Float32.self)
                var inter = [Float32](repeating: 0, count: frames * 2)
                for i in 0..<frames {
                    inter[i * 2] = lp[i]
                    inter[i * 2 + 1] = rp[i]
                }
                inter.withUnsafeBytes { out.write(Data($0)) }
            }
        }
    }

    func stream(_ stream: SCStream, didStopWithError error: Error) {
        FileHandle.standardError.write("SB_ERR: stream stopped: \(error.localizedDescription)\n".data(using: .utf8)!)
        exit(3)
    }
}

func fail(_ msg: String, _ code: Int32) -> Never {
    FileHandle.standardError.write("SB_ERR: \(msg)\n".data(using: .utf8)!)
    exit(code)
}

let tap = Tap()
let queue = DispatchQueue(label: "systap.audio")
var streamRef: SCStream?

Task {
    do {
        let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: false)
        guard let display = content.displays.first else {
            fail("no display found", 2)
        }
        let filter = SCContentFilter(display: display, excludingWindows: [])
        let conf = SCStreamConfiguration()
        conf.capturesAudio = true
        conf.excludesCurrentProcessAudio = true
        conf.sampleRate = 48000
        conf.channelCount = 2
        conf.width = 2
        conf.height = 2
        conf.minimumFrameInterval = CMTime(value: 1, timescale: 1)
        let stream = SCStream(filter: filter, configuration: conf, delegate: tap)
        try stream.addStreamOutput(tap, type: .audio, sampleHandlerQueue: queue)
        try await stream.startCapture()
        streamRef = stream
        FileHandle.standardError.write("SB_READY\n".data(using: .utf8)!)
    } catch {
        fail("capture failed (grant Screen Recording permission?): \(error.localizedDescription)", 2)
    }
}

let stopper: (Int32) -> Void = { _ in
    if let s = streamRef {
        let sem = DispatchSemaphore(value: 0)
        s.stopCapture { _ in sem.signal() }
        _ = sem.wait(timeout: .now() + 2)
    }
    exit(0)
}
signal(SIGINT, SIG_IGN)
signal(SIGTERM, SIG_IGN)
let sigint = DispatchSource.makeSignalSource(signal: SIGINT, queue: .main)
sigint.setEventHandler { stopper(SIGINT) }
sigint.resume()
let sigterm = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
sigterm.setEventHandler { stopper(SIGTERM) }
sigterm.resume()

RunLoop.main.run()
