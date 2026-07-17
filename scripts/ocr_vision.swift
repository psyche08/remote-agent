// ocr_vision.swift
//
// Local, offline OCR using Apple's Vision framework. Reads an image file and
// prints the recognized text (one observation per line) to stdout.
//
// Usage:   swift scripts/ocr_vision.swift <image-path>
// Exit:    0 ok · 2 bad args · 3 unreadable image · 4 recognition error
//
// No network, no third-party service — text never leaves the machine. This is
// the OCR worker behind BaseProvider.ocr_latest_screenshot().

import Foundation
import Vision
import AppKit

let args = CommandLine.arguments
guard args.count > 1 else {
    FileHandle.standardError.write("usage: ocr_vision.swift <image-path>\n".data(using: .utf8)!)
    exit(2)
}

let path = args[1]
guard let image = NSImage(contentsOfFile: path),
      let cgImage = image.cgImage(forProposedRect: nil, context: nil, hints: nil) else {
    FileHandle.standardError.write("cannot load image: \(path)\n".data(using: .utf8)!)
    exit(3)
}

let request = VNRecognizeTextRequest()
request.recognitionLevel = .accurate
request.usesLanguageCorrection = true
// Chinese first, then English. macOS picks the best per-region candidate.
request.recognitionLanguages = ["zh-Hans", "zh-Hant", "en-US"]

let handler = VNImageRequestHandler(cgImage: cgImage, options: [:])
do {
    try handler.perform([request])
    let observations = request.results ?? []
    let lines = observations.compactMap { obs -> String? in
        (obs as? VNRecognizedTextObservation)?.topCandidates(1).first?.string
    }
    print(lines.joined(separator: "\n"))
    exit(0)
} catch {
    FileHandle.standardError.write("ocr failed: \(error)\n".data(using: .utf8)!)
    exit(4)
}
