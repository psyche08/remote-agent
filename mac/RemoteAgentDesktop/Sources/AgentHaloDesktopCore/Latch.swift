import Foundation

/// A one-way latch: it starts open, closes once, and never reopens.
///
/// This is the Go original's closed-channel idiom made explicit. Two places
/// depend on the broadcast property, and both are load-bearing:
///
///   * A window's `done` latch. "The window is closed" has to mean "the screen
///     is confirmed locked", so a second closer waits here rather than
///     returning while the first one's relock is still in flight. A semaphore
///     would wake exactly one waiter; every waiter must be released.
///   * The controller's stop latch, which several loops poll while sleeping.
///
/// `wait(timeout:)` is the `select { case <-ch: case <-time.After(d): }` shape:
/// it sleeps for the interval but returns early once the latch closes.
final class Latch: @unchecked Sendable {
    private let condition = NSCondition()
    private var closed = false

    var isClosed: Bool {
        condition.lock()
        defer { condition.unlock() }
        return closed
    }

    func close() {
        condition.lock()
        closed = true
        condition.broadcast()
        condition.unlock()
    }

    func wait() {
        condition.lock()
        while !closed { condition.wait() }
        condition.unlock()
    }

    /// Returns true if the latch closed before the deadline.
    @discardableResult
    func wait(timeout: TimeInterval) -> Bool {
        condition.lock()
        defer { condition.unlock() }
        if closed { return true }
        _ = condition.wait(until: Date().addingTimeInterval(timeout))
        return closed
    }
}
