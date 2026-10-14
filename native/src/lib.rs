use std::borrow::Cow;
use std::io::Cursor;
use std::panic::catch_unwind;

// The initial version provides controlled self-tests only; business data interfaces belong to the image module.
#[unsafe(no_mangle)]
pub extern "C" fn pebril_native_abi() -> u32 { 1 }

fn probe_svg() -> Result<(), String> {
    let input = br#"<svg xmlns="http://www.w3.org/2000/svg" width="2" height="2"><script>alert(1)</script><rect width="2" height="2" onload="alert(1)"/></svg>"#;
    let mut output = Vec::new();
    svg_hush::Filter::new().filter(Cursor::new(input), &mut output).map_err(|e| e.to_string())?;
    let text = String::from_utf8(output).map_err(|e| e.to_string())?;
    if !text.contains("rect") || text.contains("script") || text.contains("onload") { return Err("SVG sanitization self-test failed".into()); }
    let bad = b"<svg xmlns='http://www.w3.org/2000/svg'><rect>";
    if svg_hush::Filter::new().filter(Cursor::new(bad), Vec::new()).is_ok() { return Err("Malformed SVG was not rejected".into()); }
    Ok(())
}
fn probe_gif() -> Result<(), String> {
    let mut output = Vec::new();
    {
        let mut encoder = gif::Encoder::new(&mut output, 2, 2, &[255,0,0, 0,0,255]).map_err(|e| e.to_string())?;
        encoder.set_repeat(gif::Repeat::Infinite).map_err(|e| e.to_string())?;
        for index in [0u8, 1u8] {
            let frame = gif::Frame { width:2, height:2, delay:5,
                dispose:gif::DisposalMethod::Keep,
                buffer:Cow::Owned(vec![index;4]), ..gif::Frame::default() };
            encoder.write_frame(&frame).map_err(|e|e.to_string())?;
        }
        encoder.into_inner().map_err(|e|e.to_string())?;
    }
    let mut options = gif::DecodeOptions::new(); options.set_color_output(gif::ColorOutput::RGBA);
    let mut decoder = options.read_info(Cursor::new(&output)).map_err(|e| e.to_string())?;
    if decoder.width()!=2 || decoder.height()!=2 || decoder.repeat()!=gif::Repeat::Infinite { return Err("Invalid GIF dimensions or loop count".into()); }
    let mut count=0;
    while let Some(frame)=decoder.read_next_frame().map_err(|e|e.to_string())? {
        if frame.delay!=5 || frame.width!=2 || frame.height!=2 || frame.buffer.len()!=16 { return Err("Invalid GIF frame information".into()); }
        count+=1;
    }
    if count!=2 { return Err("Invalid GIF frame count".into()); }
    Ok(())
}
#[unsafe(no_mangle)]
pub extern "C" fn pebril_native_probe() -> u32 {
    catch_unwind(|| {
        let mut bits=0;
        if probe_svg().is_ok() { bits|=1; }
        if probe_gif().is_ok() { bits|=2; }
        bits
    }).unwrap_or(0)
}

#[cfg(test)]
mod tests {
    #[test]
    fn native_library_contract() {
        assert_eq!(super::pebril_native_abi(), 1);
        super::probe_svg().expect("SVG sanitization and malformed-input rejection");
        super::probe_gif().expect("GIF frame timing and looping");
        assert_eq!(super::pebril_native_probe(), 3);
    }
}
