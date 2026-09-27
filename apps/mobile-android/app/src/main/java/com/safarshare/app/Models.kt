package com.safarshare.app

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

@Serializable
data class Problem(
    val type: String? = null,
    val title: String,
    val status: Int,
    val detail: String? = null,
)

@Serializable
data class Session(
    val token: String,
    @SerialName("token_type") val tokenType: String,
    @SerialName("expires_at") val expiresAt: String,
    val user: User,
)

@Serializable
data class User(val id: String, val role: String)

/** POST /v1/auth/otp/request — note expires_in is an integer. */
@Serializable
data class OtpRequestResponse(
    val status: String = "",
    @SerialName("expires_in") val expiresIn: Int = 0,
    val masked: String = "",
    @SerialName("dev_otp") val devOtp: String? = null, // dev echo only
)

@Serializable
data class SearchRequest(
    @SerialName("pickup_lat") val pickupLat: Double,
    @SerialName("pickup_lng") val pickupLng: Double,
    @SerialName("drop_lat") val dropLat: Double,
    @SerialName("drop_lng") val dropLng: Double,
    val date: String,
    val time: String? = null,
    val seats: Int = 1,
)

@Serializable
data class ScoreParts(
    val detour: Double = 0.0,
    val time: Double = 0.0,
    val rating: Double = 0.0,
    val price: Double = 0.0,
)

@Serializable
data class Ride(
    @SerialName("ride_instance_id") val rideInstanceId: String,
    @SerialName("route_id") val routeId: String,
    @SerialName("driver_id") val driverId: String,
    @SerialName("driver_rating") val driverRating: Double,
    @SerialName("origin_label") val originLabel: String,
    @SerialName("dest_label") val destLabel: String,
    @SerialName("departure_time") val departureTime: String,
    @SerialName("duration_min") val durationMin: Int,
    @SerialName("distance_km") val distanceKm: Double,
    @SerialName("seats_free") val seatsFree: Int,
    val fare: Double,
    val total: Double,
    @SerialName("detour_km") val detourKm: Double,
    @SerialName("detour_min") val detourMin: Double,
    @SerialName("price_estimate") val priceEstimate: Double,
    val score: Double,
    @SerialName("score_parts") val scoreParts: ScoreParts = ScoreParts(),
)

@Serializable
data class SearchResponse(
    val results: List<Ride> = emptyList(),
    val count: Int = 0,
    val weights: ScoreParts = ScoreParts(),
    @SerialName("radius_m") val radiusM: Double = 0.0,
)

@Serializable
data class CreateBookingRequest(
    @SerialName("ride_instance_id") val rideInstanceId: String,
    val seats: Int,
)

@Serializable
data class PaymentIntent(
    val id: String,
    val amount: Double,
    val status: String,
    val provider: String? = null,
)

@Serializable
data class Booking(
    val id: String,
    @SerialName("ride_instance_id") val rideInstanceId: String,
    @SerialName("passenger_id") val passengerId: String,
    val seats: Int,
    val status: String,
    val fare: Double,
    val total: Double,
    @SerialName("created_at") val createdAt: String,
    val payment: PaymentIntent? = null,
)

@Serializable
data class BookingsResponse(val bookings: List<Booking> = emptyList())
