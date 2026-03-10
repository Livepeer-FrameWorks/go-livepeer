#!/usr/bin/env bash
echo 'WARNING: downloading and executing lpms/install_ffmpeg.sh, use it directly in case of issues'
curl https://raw.githubusercontent.com/Livepeer-FrameWorks/lpms/4716251a7a457bb663629cd5115f39a7a667e736/install_ffmpeg.sh | bash -s $1
