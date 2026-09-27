import SwiftUI

@main
struct SafarShareApp: App {
    @StateObject private var session = SessionStore()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(session)
        }
    }
}

/// App-wide auth state. The bearer token is persisted in the keychain-backed
/// UserDefaults store (MVP; production should use Keychain directly).
final class SessionStore: ObservableObject {
    @Published var token: String? {
        didSet {
            if let t = token {
                UserDefaults.standard.set(t, forKey: "token")
            } else {
                UserDefaults.standard.removeObject(forKey: "token")
            }
            Api.token = token
        }
    }

    init() {
        let t = UserDefaults.standard.string(forKey: "token")
        token = t
        Api.token = t
    }

    func clear() {
        token = nil
    }
}
