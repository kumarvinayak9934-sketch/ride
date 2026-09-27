import SwiftUI

enum Screen: String, CaseIterable {
    case search = "Search"
    case bookings = "My bookings"
    case login = "Login"
}

struct RootView: View {
    @EnvironmentObject var session: SessionStore
    @State private var screen: Screen = .search
    @State private var message: String?

    var body: some View {
        NavigationStack {
            Group {
                switch screen {
                case .search:
                    SearchView(onMessage: show)
                case .bookings:
                    BookingsView(onMessage: show)
                case .login:
                    LoginView { msg in
                        session.token = Api.token
                        show(msg)
                        screen = .search
                    }
                }
            }
            .navigationTitle("Safar Share")
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    if screen != .search {
                        Button(action: { screen = .search }) {
                            Image(systemName: "chevron.left")
                        }
                    }
                }
                ToolbarItem(placement: .navigationBarTrailing) {
                    HStack {
                        Button("Login") { screen = .login }
                        Button("Bookings") { screen = .bookings }
                    }
                }
            }
            .safeAreaInset(edge: .bottom) {
                if let message {
                    Text(message)
                        .font(.footnote)
                        .padding(8)
                        .frame(maxWidth: .infinity)
                        .background(Color(.systemGray6))
                        .onTapGesture { self.message = nil }
                }
            }
        }
    }

    private func show(_ msg: String) {
        message = msg
    }
}

// MARK: - Login (OTP)

struct LoginView: View {
    var onDone: (String) -> Void
    @State private var phone = "+91"
    @State private var code = ""
    @State private var sent = false
    @State private var busy = false
    @FocusState private var focused: Bool

    var body: some View {
        Form {
            Section {
                if !sent {
                    TextField("Mobile (+91…)", text: $phone)
                        .keyboardType(.phonePad)
                        .textContentType(.telephoneNumber)
                    Button(busy ? "Sending…" : "Send OTP") { send() }
                        .disabled(busy || phone.isEmpty)
                } else {
                    SecureField("6-digit code", text: $code)
                        .keyboardType(.numberPad)
                    Button(busy ? "Verifying…" : "Verify & continue") { verify() }
                        .disabled(busy || code.count != 6)
                    Button("Change number") { sent = false }
                }
            } header: {
                Text("Log in with OTP")
            } footer: {
                Text("We store an HMAC hash and an encrypted number only (DPDP).")
            }
        }
    }

    private func send() {
        busy = true
        Task {
            defer { busy = false }
            do {
                struct OtpRequest: Encodable { let phone: String }
                _ = try await Api.request(
                    "POST", "/v1/auth/otp/request",
                    as: OtpRequestResponse.self,
                    body: OtpRequest(phone: phone)
                )
                sent = true
            } catch {
                onDone(error.localizedDescription)
            }
        }
    }

    private func verify() {
        busy = true
        Task {
            defer { busy = false }
            struct OtpVerify: Encodable {
                let phone: String
                let code: String
                let ageConfirmed: Bool
            }
            do {
                let session = try await Api.request(
                    "POST", "/v1/auth/otp/verify",
                    as: Session.self,
                    body: OtpVerify(phone: phone, code: code, ageConfirmed: true)
                )
                Api.token = session.token
                onDone("Welcome!")
            } catch {
                onDone(error.localizedDescription)
            }
        }
    }
}

// MARK: - Search

struct SearchView: View {
    var onMessage: (String) -> Void
    @State private var pickupLat = "12.9352"
    @State private var pickupLng = "77.6245"
    @State private var dropLat = "12.9698"
    @State private var dropLng = "77.7500"
    @State private var date = ISO8601DateFormatter()
        .string(from: Date().addingTimeInterval(86_400))
        .prefix(10)
        .description
    @State private var seats = "1"
    @State private var rides: [Ride] = []
    @State private var searched = false
    @State private var busy = false

    var body: some View {
        Form {
            Section("Popular Bengaluru corridor preset") {
                TextField("Pickup lat", text: $pickupLat).keyboardType(.decimalPad)
                TextField("Pickup lng", text: $pickupLng).keyboardType(.decimalPad)
                TextField("Drop lat", text: $dropLat).keyboardType(.decimalPad)
                TextField("Drop lng", text: $dropLng).keyboardType(.decimalPad)
                TextField("Date (YYYY-MM-DD)", text: $date)
                TextField("Seats", text: $seats).keyboardType(.numberPad)
            }
            Section {
                Button(busy ? "Searching…" : "Search rides") { search() }
                    .disabled(busy)
            }
            if searched && rides.isEmpty {
                Section {
                    Text("No rides on this corridor for that date — try another day.")
                        .foregroundStyle(.secondary)
                }
            }
            ForEach(rides) { ride in
                Section {
                    VStack(alignment: .leading, spacing: 6) {
                        Text("\(ride.originLabel) → \(ride.destLabel)")
                            .font(.headline)
                        Text("\(ride.departureTime) · \(ride.durationMin) min · \(ride.distanceKm, specifier: "%.1f") km · ★ \(ride.driverRating, specifier: "%.1f") · \(ride.seatsFree) free")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        HStack {
                            Text("₹\(ride.total, specifier: "%.0f") / seat")
                                .font(.headline)
                            Spacer()
                            Button("Book") { book(ride) }
                                .buttonStyle(.borderedProminent)
                        }
                    }
                }
            }
        }
    }

    private func search() {
        busy = true
        Task {
            defer { busy = false }
            let body = SearchRequest(
                pickupLat: Double(pickupLat) ?? 0,
                pickupLng: Double(pickupLng) ?? 0,
                dropLat: Double(dropLat) ?? 0,
                dropLng: Double(dropLng) ?? 0,
                date: String(date),
                time: nil,
                seats: Int(seats) ?? 1
            )
            do {
                let r = try await Api.request(
                    "POST", "/v1/rides/search",
                    as: SearchResponse.self,
                    body: body
                )
                rides = r.results
                searched = true
            } catch {
                onMessage(error.localizedDescription)
            }
        }
    }

    private func book(_ ride: Ride) {
        Task {
            let body = CreateBookingRequest(
                rideInstanceId: ride.rideInstanceId,
                seats: Int(seats) ?? 1
            )
            do {
                _ = try await Api.request(
                    "POST", "/v1/bookings",
                    as: Booking.self,
                    body: body,
                    idempotencyKey: UUID().uuidString
                )
                onMessage("Booked! Check My bookings.")
            } catch {
                onMessage("Booking failed — \(error.localizedDescription)")
            }
        }
    }
}

// MARK: - Bookings

struct BookingsView: View {
    var onMessage: (String) -> Void
    @State private var bookings: [Booking] = []
    @State private var loaded = false

    var body: some View {
        Group {
            if !loaded {
                ProgressView()
            } else if bookings.isEmpty {
                Text("No bookings yet.").foregroundStyle(.secondary)
            } else {
                List(bookings) { b in
                    VStack(alignment: .leading, spacing: 4) {
                        HStack {
                            Text("\(b.seats) seat(s) · ₹\(b.total, specifier: "%.0f")")
                                .font(.headline)
                            Spacer()
                            Text(b.status)
                                .font(.caption)
                                .padding(.horizontal, 8)
                                .padding(.vertical, 3)
                                .background(Capsule().fill(Color(.systemGray5)))
                        }
                        Text("…\(b.id.prefix(8)) · \(b.createdAt.formatted(date: .abbreviated, time: .shortened))")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                .listStyle(.inset)
            }
        }
        .task { await load() }
        .toolbar {
            Button("Refresh") { Task { await load() } }
        }
    }

    private func load() async {
        do {
            let r = try await Api.request("GET", "/v1/bookings", as: BookingsResponse.self)
            bookings = r.bookings
            loaded = true
        } catch {
            onMessage(error.localizedDescription)
            loaded = true
        }
    }
}
