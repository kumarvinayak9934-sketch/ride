import type { Config } from "tailwindcss";

const config: Config = {
  content: ["./app/**/*.{ts,tsx}", "./components/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        brand: {
          50: "#eef7ff",
          100: "#d9edff",
          500: "#1673d1",
          600: "#0f5cab",
          700: "#0d4a8a",
          900: "#0b2f57",
        },
      },
    },
  },
  plugins: [],
};

export default config;
