package com.safarshare.app

import android.content.Context
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val prefs = getSharedPreferences("safar", Context.MODE_PRIVATE)
        Api.token = prefs.getString("token", null)

        setContent {
            MaterialTheme(colorScheme = lightColorScheme(primary = BrandBlue)) {
                SafarApp(onSession = { token ->
                    prefs.edit().putString("token", token).apply()
                    Api.token = token
                })
            }
        }
    }
}

private val BrandBlue = androidx.compose.ui.graphics.Color(0xFF0F5CAB)

private enum class Screen { Login, Search, Bookings }

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SafarApp(onSession: (String) -> Unit) {
    var screen by remember { mutableStateOf(Screen.Search) }
    val snack = remember { SnackbarHostState() }
    val scope = rememberCoroutineScope()

    fun toast(msg: String) = scope.launch { snack.showSnackbar(msg) }

    Scaffold(
        snackbarHost = { SnackbarHost(snack) },
        topBar = {
            TopAppBar(
                title = { Text("Safar Share") },
                navigationIcon = {
                    if (screen != Screen.Search) {
                        IconButton(onClick = { screen = Screen.Search }) {
                            Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                        }
                    }
                },
                actions = {
                    TextButton(onClick = { screen = Screen.Login }) { Text("Login") }
                    TextButton(onClick = { screen = Screen.Bookings }) { Text("Bookings") }
                },
            )
        },
    ) { pad ->
        when (screen) {
            Screen.Login -> LoginScreen(Modifier.padding(pad), onSession) {
                toast(it); screen = Screen.Search
            }
            Screen.Search -> SearchScreen(Modifier.padding(pad), toast)
            Screen.Bookings -> BookingsScreen(Modifier.padding(pad), toast)
        }
    }
}

@Composable
fun LoginScreen(modifier: Modifier = Modifier, onSession: (String) -> Unit, onDone: (String) -> Unit) {
    var phone by remember { mutableStateOf("+91") }
    var code by remember { mutableStateOf("") }
    var sent by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    Column(modifier = modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        Text("Log in with OTP", style = MaterialTheme.typography.titleLarge)
        Text(
            "We store an HMAC hash and an encrypted number only (DPDP).",
            style = MaterialTheme.typography.bodySmall,
        )

        if (!sent) {
            OutlinedTextField(
                value = phone, onValueChange = { phone = it },
                label = { Text("Mobile (+91…)") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Phone),
                modifier = Modifier.fillMaxWidth(),
            )
            Button(
                enabled = !busy,
                onClick = {
                    busy = true
                    scope.launch {
                        val body = """{"phone":"$phone"}"""
                        when (
                            val r = Api.request(
                                "POST", "/v1/auth/otp/request",
                                OtpRequestResponse.serializer(), body,
                            )
                        ) {
                            is Api.Result.Ok -> {
                                sent = true
                                // Dev echo (OTPV_DEV_ECHO): prefill when the server sends it.
                                r.value.devOtp?.let { code = it }
                            }
                            is Api.Result.Err -> onDone(r.message)
                        }
                        busy = false
                    }
                },
                modifier = Modifier.fillMaxWidth(),
            ) { Text(if (busy) "Sending…" else "Send OTP") }
        } else {
            OutlinedTextField(
                value = code, onValueChange = { code = it.filter(Char::isDigit).take(6) },
                label = { Text("6-digit code") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.NumberPassword),
                visualTransformation = PasswordVisualTransformation(),
                modifier = Modifier.fillMaxWidth(),
            )
            Button(
                enabled = !busy && code.length == 6,
                onClick = {
                    busy = true
                    scope.launch {
                        val body = """{"phone":"$phone","code":"$code","age_confirmed":true}"""
                        when (val r = Api.request("POST", "/v1/auth/otp/verify", Session.serializer(), body)) {
                            is Api.Result.Ok -> {
                                Api.token = r.value.token
                                onSession(r.value.token)
                                onDone("Welcome!")
                            }
                            is Api.Result.Err -> onDone(r.message)
                        }
                        busy = false
                    }
                },
                modifier = Modifier.fillMaxWidth(),
            ) { Text(if (busy) "Verifying…" else "Verify & continue") }

            Spacer(Modifier.height(4.dp))
            TextButton(onClick = { sent = false }) { Text("Change number") }
        }
    }
}

@Composable
fun SearchScreen(modifier: Modifier = Modifier, onError: (String) -> Unit) {
    // Popular Bengaluru corridor preset (matches the web app).
    var pickupLat by remember { mutableStateOf("12.9352") }
    var pickupLng by remember { mutableStateOf("77.6245") }
    var dropLat by remember { mutableStateOf("12.9698") }
    var dropLng by remember { mutableStateOf("77.7500") }
    var date by remember {
        mutableStateOf(
            java.time.LocalDate.now().plusDays(1).toString(),
        )
    }
    var seats by remember { mutableStateOf("1") }
    var rides by remember { mutableStateOf<List<Ride>>(emptyList()) }
    var busy by remember { mutableStateOf(false) }
    var searched by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    Column(
        modifier = modifier.fillMaxSize().padding(16.dp).verticalScroll(rememberScrollState()),
        verticalArrangement = Arrangement.spacedBy(10.dp),
    ) {
        Text("Find a ride", style = MaterialTheme.typography.titleLarge)

        OutlinedTextField(pickupLat, { pickupLat = it }, label = { Text("Pickup lat") }, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(pickupLng, { pickupLng = it }, label = { Text("Pickup lng") }, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(dropLat, { dropLat = it }, label = { Text("Drop lat") }, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(dropLng, { dropLng = it }, label = { Text("Drop lng") }, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(date, { date = it }, label = { Text("Date (YYYY-MM-DD)") }, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(seats, { seats = it.filter(Char::isDigit) }, label = { Text("Seats") }, modifier = Modifier.fillMaxWidth())

        Button(
            enabled = !busy,
            onClick = {
                busy = true
                scope.launch {
                    val body = """
                        {"pickup_lat":${pickupLat.toDoubleOrNull() ?: 0},
                         "pickup_lng":${pickupLng.toDoubleOrNull() ?: 0},
                         "drop_lat":${dropLat.toDoubleOrNull() ?: 0},
                         "drop_lng":${dropLng.toDoubleOrNull() ?: 0},
                         "date":"$date",
                         "seats":${seats.toIntOrNull() ?: 1}}
                    """.trimIndent()
                    when (val r = Api.request("POST", "/v1/rides/search", SearchResponse.serializer(), body)) {
                        is Api.Result.Ok -> { rides = r.value.results; searched = true }
                        is Api.Result.Err -> onError(r.message)
                    }
                    busy = false
                }
            },
            modifier = Modifier.fillMaxWidth(),
        ) { Text(if (busy) "Searching…" else "Search rides") }

        if (busy) Spacer(Modifier.height(8.dp))

        if (searched && rides.isEmpty()) {
            Card(Modifier.fillMaxWidth()) {
                Text(
                    "No rides on this corridor for that date — try another day.",
                    Modifier.padding(16.dp),
                )
            }
        }

        rides.forEach { ride ->
            RideCard(ride) {
                scope.launch {
                    val body =
                        """{"ride_instance_id":"${ride.rideInstanceId}","seats":${seats.toIntOrNull() ?: 1}}"""
                    when (
                        Api.request(
                            "POST", "/v1/bookings", Booking.serializer(), body,
                            idempotencyKey = java.util.UUID.randomUUID().toString(),
                        )
                    ) {
                        is Api.Result.Ok -> onError("Booked! Check My bookings.")
                        is Api.Result.Err -> onError("Booking failed — see Bookings")
                    }
                }
            }
        }
    }
}

@Composable
private fun RideCard(ride: Ride, onBook: () -> Unit) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text("${ride.originLabel} → ${ride.destLabel}", style = MaterialTheme.typography.titleMedium)
            Text(
                "${ride.departureTime} · ${ride.durationMin} min · " +
                    "${"%.1f".format(ride.distanceKm)} km · ★ ${"%.1f".format(ride.driverRating)} · " +
                    "${ride.seatsFree} free",
                style = MaterialTheme.typography.bodySmall,
            )
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text(
                    "₹${"%.0f".format(ride.total)} / seat",
                    style = MaterialTheme.typography.titleMedium,
                )
                Button(onClick = onBook) { Text("Book") }
            }
        }
    }
}

@Composable
fun BookingsScreen(modifier: Modifier = Modifier, onError: (String) -> Unit) {
    var bookings by remember { mutableStateOf<List<Booking>>(emptyList()) }
    var loaded by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    fun load() {
        scope.launch {
            when (val r = Api.request("GET", "/v1/bookings", BookingsResponse.serializer())) {
                is Api.Result.Ok -> { bookings = r.value.bookings; loaded = true }
                is Api.Result.Err -> { onError(r.message); loaded = true }
            }
        }
    }

    androidx.compose.runtime.LaunchedEffect(Unit) { load() }

    Column(modifier = modifier.fillMaxSize().padding(16.dp)) {
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            Text("My bookings", style = MaterialTheme.typography.titleLarge)
            TextButton(onClick = { load() }) { Text("Refresh") }
        }
        Spacer(Modifier.height(8.dp))

    if (!loaded) {
        CircularProgressIndicator()
    } else if (bookings.isEmpty()) {
            Text("No bookings yet.", style = MaterialTheme.typography.bodyMedium)
        } else {
            LazyColumn(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                items(bookings, key = { it.id }) { b ->
                    Card(Modifier.fillMaxWidth()) {
                        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                                Text("${b.seats} seat(s) · ₹${"%.0f".format(b.total)}")
                                Text(b.status, style = MaterialTheme.typography.labelMedium)
                            }
                            Text("…${b.id.take(8)} · ${b.createdAt.take(16).replace('T', ' ')}",
                                style = MaterialTheme.typography.bodySmall)
                        }
                    }
                }
            }
        }
    }
}
