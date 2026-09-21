/** @type {import('next').NextConfig} */
const nextConfig = {
  // Traces only the files a production server needs into .next/standalone,
  // so the runtime image the Dockerfile builds needs no node_modules copy.
  output: 'standalone',
};

module.exports = nextConfig;
