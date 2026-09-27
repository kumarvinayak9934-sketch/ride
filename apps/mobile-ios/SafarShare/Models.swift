import Foundation

// Codable mirrors of the API contract in `docs/openapi.yaml`.
// JSONDecoder/Encoder use snake_case conversion, so property names are camelCase.

struct Problem: Codable {
    let type: String
    let title: String
    let status: Int
    let detail: String?
}

// MARK: - Auth

struct OtpRequestResponse: Codable {
    let status: String
    let expiresIn: Int
    let masked: String
    let devOtp: String? // dev echo only (OTPV_DEV_ECHO)
}

struct User: Codable {
    let id: String
    let role: String
}

struct Session: Codable {
    let token: String
    let tokenType: String
    let expiresAt: String
    let user: User
}

// MARK: - Search

struct SearchRequest: Codable {
    let pickupLat: Double
    let pickupLng: Double
    let dropLat: Double
    let dropLng: Double
    let date: String
    let time: String?
    let seats: Int
}

struct ScoreParts: Codable {
    let detour: Double
    let time: Double
    let rating: Double
    let price: Double
}

struct Ride: Codable, Identifiable {
    var id: String { rideInstanceId }
    let rideInstanceId: String
    let routeId: String
    let driverId: String
    let driverRating: Double
    let originLabel: String
    let destLabel: String
    let departureTime: String
    let durationMin: Int
    let distanceKm: Double
    let seatsFree: Int
    let fare: Double
    let total: Double
    let detourKm: Double
    let detourMin: Double
    let priceEstimate: Double
    let score: Double
    let scoreParts: ScoreParts
}

struct SearchResponse: Codable {
    let results: [Ride]
    let count: Int
}

// MARK: - Bookings

struct CreateBookingRequest: Codable {
    let rideInstanceId: String
    let seats: Int
}

struct PaymentIntent: Codable {
    let id: String
    let amount: Double
    let status: String
    let provider: String?
    let upiIntent: String?
}

struct Booking: Codable, Identifiable {
    let id: String
    let rideInstanceId: String
    let passengerId: String
    let seats: Int
    let status: String
    let fare: Double
    let total: Double
    let createdAt: Date
    let payment: PaymentIntent?
}

struct BookingsResponse: Codable {
    let bookings: [Booking]
}
