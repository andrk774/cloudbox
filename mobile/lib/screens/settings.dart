import 'package:flutter/material.dart';

import '../api.dart';
import 'package:shared_preferences/shared_preferences.dart';

import '../autoupload.dart';
import '../background.dart';
import '../storage.dart';
import 'login.dart';

/// Настройки: учётная запись, место, автозагрузка фото, выход.
class SettingsScreen extends StatefulWidget {
  const SettingsScreen({super.key, required this.api});
  final CloudBoxApi api;
  @override
  State<SettingsScreen> createState() => _SettingsScreenState();
}

class _SettingsScreenState extends State<SettingsScreen> {
  User? _me;
  bool _auto = false;
  bool _wifiOnly = true;
  bool _chargingOnly = false;
  DateTime? _lastRun;
  String? _status;

  @override
  void initState() {
    super.initState();
    widget.api.me().then((u) => mounted ? setState(() => _me = u) : null).catchError((_) {});
    _loadAuto();
  }

  Future<void> _loadAuto() async {
    final p = await SharedPreferences.getInstance();
    await p.reload();
    final auto = await AutoUpload.enabled(), wifi = await Background.wifiOnly(), charging = await Background.chargingOnly();
    final last = p.getInt(Storage.autoUploadLastRunKey);
    if (!mounted) return;
    setState(() {
      _auto = auto;
      _wifiOnly = wifi;
      _chargingOnly = charging;
      _lastRun = last == null ? null : DateTime.fromMillisecondsSinceEpoch(last);
    });
  }

  Future<void> _toggle(bool on) async {
    if (!on) {
      await AutoUpload.disable();
      setState(() => _auto = false);
      return;
    }
    final all = await showDialog<bool>(
      context: context,
      builder: (ctx) => AlertDialog(
        title: const Text('Автозагрузка фото'),
        content: const Text('Фото и видео будут загружаться в папку «Фото/Камера» при каждом открытии приложения.\n\n'
            'Загрузить и уже снятые фото или только новые?'),
        actions: [
          TextButton(onPressed: () => Navigator.pop(ctx), child: const Text('Отмена')),
          TextButton(onPressed: () => Navigator.pop(ctx, false), child: const Text('Только новые')),
          FilledButton(onPressed: () => Navigator.pop(ctx, true), child: const Text('Все')),
        ],
      ),
    );
    if (all == null) return;
    if (!await AutoUpload.enable(allExisting: all)) {
      setState(() => _status = 'Нет доступа к фото — разрешите его в настройках телефона');
      return;
    }
    setState(() => _auto = true);
    _run();
  }

  Future<void> _run() async {
    setState(() => _status = 'Ищу новые фото…');
    final r = await AutoUpload.run(widget.api, status: (s) => mounted ? setState(() => _status = s) : null);
    if (!mounted) return;
    setState(() => _status = r.message ??
        (r.uploaded + r.failed == 0 ? 'Новых фото нет' : 'Загружено: ${r.uploaded}${r.failed > 0 ? ', не удалось: ${r.failed}' : ''}'));
    _loadAuto();
  }

  Future<void> _logout() async {
    await Background.cancel(); // иначе фоновая задача продолжала бы просыпаться без аккаунта
    await Storage.clear();
    if (!mounted) return;
    Navigator.of(context).pushAndRemoveUntil(MaterialPageRoute(builder: (_) => const LoginScreen()), (_) => false);
  }

  @override
  Widget build(BuildContext context) {
    final u = _me;
    return Scaffold(
      appBar: AppBar(title: const Text('Настройки')),
      body: ListView(children: [
        ListTile(
          leading: const Icon(Icons.person),
          title: Text(u?.username ?? widget.api.user),
          subtitle: Text(widget.api.base.toString()),
        ),
        if (u != null)
          ListTile(
            leading: const Icon(Icons.storage),
            title: Text('Занято ${formatSize(u.usedBytes)}${u.quotaBytes > 0 ? ' из ${formatSize(u.quotaBytes)}' : ''}'),
            subtitle: u.quotaBytes > 0 ? LinearProgressIndicator(value: (u.usedBytes / u.quotaBytes).clamp(0, 1).toDouble()) : null,
          ),
        if (widget.api.insecure)
          const ListTile(leading: Icon(Icons.warning, color: Colors.orange), title: Text('Подключение без HTTPS'),
              subtitle: Text('Пароль и файлы передаются открытым текстом — используйте только в домашней сети')),
        const Divider(),
        SwitchListTile(
          secondary: const Icon(Icons.photo_library),
          title: const Text('Автозагрузка фото и видео'),
          subtitle: Text(Background.supported
              ? 'В «Фото/Камера/<год>»: в фоне примерно раз в 15 минут и при открытии приложения'
              : 'В «Фото/Камера/<год>» при открытии приложения'),
          value: _auto,
          onChanged: _toggle,
        ),
        if (_auto && Background.supported) ...[
          SwitchListTile(
            key: const Key('wifi-only'),
            secondary: const Icon(Icons.wifi),
            title: const Text('Только по Wi-Fi'),
            subtitle: const Text('Не тратить мобильный интернет (точнее — только безлимитные сети)'),
            value: _wifiOnly,
            onChanged: (v) async {
              await Background.setConditions(wifiOnly: v);
              setState(() => _wifiOnly = v);
            },
          ),
          SwitchListTile(
            key: const Key('charging-only'),
            secondary: const Icon(Icons.battery_charging_full),
            title: const Text('Только при зарядке'),
            subtitle: const Text('Фоновая загрузка — когда телефон подключён к зарядке'),
            value: _chargingOnly,
            onChanged: (v) async {
              await Background.setConditions(chargingOnly: v);
              setState(() => _chargingOnly = v);
            },
          ),
          const Padding(
            padding: EdgeInsets.symmetric(horizontal: 16),
            child: Text('Android сам решает, когда запускать фоновые задачи: в режиме энергосбережения или при '
                'агрессивной оптимизации батареи (Xiaomi, Huawei, Samsung) загрузка может откладываться. '
                'Если фото не уходят в фоне, разрешите CloudBox работу без ограничений в настройках батареи.',
                style: TextStyle(fontSize: 12)),
          ),
        ],
        if (_auto) ListTile(leading: const Icon(Icons.sync), title: const Text('Загрузить сейчас'), onTap: _run,
            subtitle: _lastRun == null ? null : Text('Последняя проверка: ${_fmt(_lastRun!)}')),
        if (_status != null) Padding(padding: const EdgeInsets.symmetric(horizontal: 16), child: Text(_status!)),
        const Divider(),
        ListTile(leading: const Icon(Icons.logout), title: const Text('Выйти'), onTap: _logout),
      ]),
    );
  }
}

String _fmt(DateTime d) {
  String two(int v) => v.toString().padLeft(2, '0');
  return '${two(d.day)}.${two(d.month)} ${two(d.hour)}:${two(d.minute)}';
}
