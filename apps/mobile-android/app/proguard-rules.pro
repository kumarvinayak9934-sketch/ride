# Kotlinx serialization models are reflected via generated serializers only.
-keepattributes *Annotation*, InnerClasses
-dontnote kotlinx.serialization.**
-keepclassmembers class com.safarshare.app.** {
    *** Companion;
}
-keepclasseswithmembers class com.safarshare.app.** {
    kotlinx.serialization.KSerializer serializer(...);
}
