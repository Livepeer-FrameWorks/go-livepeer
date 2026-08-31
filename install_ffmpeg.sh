#!/usr/bin/env bash
echo 'WARNING: downloading and executing lpms/install_ffmpeg.sh, use it directly in case of issues'
curl https://raw.githubusercontent.com/Livepeer-FrameWorks/lpms/297b5f291dc4ffe24e4bdded6fd78dfe1446e624/install_ffmpeg.sh | bash -s $1
